# devplat-agent

Go agent for devplat's Firecracker microVM orchestration. One binary, runs
directly on each data-plane host (Host A, Host B, ...), talks to Firecracker
over its REST API via [firecracker-go-sdk](https://github.com/firecracker-microvm/firecracker-go-sdk),
and is controlled by the scheduler (in `devplat-backend`) over the WireGuard
tunnel.

```
Scheduler (VPS, over WireGuard 10.10.0.x)
    │  POST/DELETE/GET /vms, GET /health — Bearer <agent token>
    ▼
devplat-agent (this repo, one per host)
    │  firecracker-go-sdk over the Firecracker REST API (unix socket)
    ▼
Firecracker microVM — unmodified Docker daemon on :2375
```

## Network design

Each VM gets its own tap device and a private point-to-point `/30`:

- **Tap naming**: `fc-tap-<slot>`, where `slot` is a per-host integer index
  (0..capacity). Slots are the unit of capacity accounting — see below.
- **IP addressing**: `TAP_IP_BASE` (e.g. `172.20.0.0`) is the first address
  of this host's `/16` VM pool. Slot `N` gets the `/30` at offset `N*4`:
  `.1` = tap device / VM's gateway (host side), `.2` = the VM's own address.
  Deterministic and collision-free up to ~16000 slots per host — see
  `internal/vmmanager/network.go:deriveNetConfig`.
- **Docker port mapping**: `DOCKER_PORT_BASE + slot` on the host is
  `iptables`-DNAT'd to `<VM's .2 address>:2375`. Only `WIREGUARD_CIDR` may
  reach it — enforced both by DNAT source-matching and an explicit `DROP`
  for everyone else, in addition to this agent's own API only binding the
  WireGuard interface in the first place.
- **Container-published ports** (Testcontainers port mapping): ports Docker
  publishes *inside* the guest get no host-side DNAT — they're ephemeral
  and per-container, so pre-provisioning is impossible. Instead the agent
  exposes `GET /vms/{id}/proxy/{port}` (same Bearer auth as everything
  else): after an HTTP `Upgrade`, the connection becomes a raw TCP pipe to
  `<guest IP>:<port>` over the tap link. The backend's per-port tunnel
  route (`/environments/:id/tunnel/:port`) is its only caller and applies
  the same team-ownership check as the Docker-API tunnel; the guest IP is
  derived strictly from the VM's slot, so a caller can pick a port but
  never another VM.
- **Egress**: a `tc` token-bucket bandwidth cap per tap device
  (`BANDWIDTH_CAP_MBIT`). No egress allowlist/blocklist yet — flagged as a
  known gap below, matching the work order's "first cut" scope (a bandwidth
  cap + blocking unsolicited inbound is what was asked for).
- **Capacity**: a raw resource budget, not a fixed count of equal slots. VMs
  are now variable-sized — each `POST /vms` carries `vcpu`/`ram_mb` (the
  requesting team's plan cap, set by the scheduler), and a VM is admitted only
  if `sum(running vcpu) + vcpu ≤ detected_cpu` **and**
  `sum(running ram) + ram_mb ≤ detected_ram − HOST_RESERVED_RAM_MB`. The slot
  index (for tap/network derivation) is still assigned lowest-free, bounded by
  the CPU count. When registering a host via `devplat-backend`'s
  `POST /admin/hosts`, use the post-reservation capacity for
  `cpuTotal`/`ramTotalMb` — the scheduler's own accounting needs to agree with
  what this agent reports on `GET /health`, or the two will drift.

## Storage

No Firecracker snapshots yet (explicitly a later phase, see the work
order) — VM creation copies the versioned golden image
(`image/build-golden-image.sh`) into a private file per VM
(`VM_STATE_DIR/<vm_id>/rootfs.ext4`), deleted on destroy. Kept simple on
purpose: nothing shared between VMs, no COW bugs to chase. The image
directory is versioned (`images/<version>/rootfs.ext4` +
`images/current` symlink) specifically so adding snapshot support later is
additive, not a rewrite.

## Reaper

A background loop (`REAPER_INTERVAL_SECONDS`) destroys any VM past
`CreatedAt + TTL`, independent of Testcontainers' own client-side Ryuk
cleanup — a crashed or misbehaving client must not leak a VM forever. Runs
in the same process as the API server, no separate service.

## Heartbeat: why HTTP to the scheduler, not a direct DB write

Agents have no route to Postgres — it has no public port, and Host A/B are
separate hardware behind WireGuard, not part of the VPS's Docker network.
So `internal/heartbeat` POSTs to the backend's public
`/internal/hosts/heartbeat` (over the internet, not the tunnel — Host A/B
have their own direct connections) using the same shared secret as inbound
auth. The backend's scheduler *also* independently polls each agent's
`GET /health` over the tunnel (see `devplat-backend`'s
`scheduler/healthPoller.ts`) — deliberately redundant: the poll is the
authoritative reconciler for capacity accounting, the push heartbeat is a
simpler "I'm alive" signal that still works if the poll direction has a
transient issue.

## Deploying a host

1. **Build the golden image** (on an x86_64 build machine with root —
   needs loop devices, mount, chroot):
   ```bash
   sudo ./image/build-golden-image.sh v1 /opt/devplat/agent/images
   ./image/fetch-kernel.sh   # -> /opt/devplat/agent/images/vmlinux-5.10.239.bin
   ```
   Copy `/opt/devplat/agent/images/` to the target host (or build directly
   on it). The kernel is the Firecracker project's own CI guest kernel
   5.10.239 — see fetch-kernel.sh's header for why (container networking
   inside the guest needs kernel netfilter/bridge/veth support the old
   quickstart 4.14 kernel didn't have) and which iptables backend it
   implies (legacy x_tables; the image installs `iptables-legacy` and
   init.sh switches to it automatically).
2. **Registry cache** (first thing that needs Docker on this host):
   ```bash
   cd deploy && docker compose -f docker-compose.registry-cache.yml up -d
   ```
3. **Register the host** with the backend (as a platform admin):
   ```bash
   curl -X POST https://api.devplat.ch/admin/hosts \
     -H "Authorization: Bearer <session-jwt>" -H "content-type: application/json" \
     -d '{"name":"host-a","agentEndpoint":"http://10.10.0.2:7777","wireguardIp":"10.10.0.2","cpuTotal":11,"ramTotalMb":114688}'
   ```
   Save the returned `agentToken` — shown once.
4. **Deploy the agent**: build the binary, copy `.env.example` to
   `/opt/devplat/agent/.env` and fill it in (including the token from step
   3), install the systemd unit:
   ```bash
   go build -o /usr/local/bin/devplat-agent ./cmd/agent
   sudo cp deploy/devplat-agent.service /etc/systemd/system/
   sudo systemctl enable --now devplat-agent
   ```

Repeat with a different `TAP_IP_BASE`/`agentEndpoint` for each additional
host.

## Testing

```bash
go build ./...
go vet ./...
go test ./...
```

`internal/vmmanager` has unit tests for the slot/capacity/TTL/reconciliation
logic against a fake `Backend` (no real system calls). The real
`FirecrackerBackend` (actual Firecracker boot, tap devices, iptables,
cgroups) is **not exercised by any test in this repo** — it needs
`/dev/kvm`, root, and a golden image, none of which are available in a
CI/dev sandbox. It must be verified against the acceptance checklist on
real hardware (Host A/B).

## Connect latency: the ~15s dockerd stall

`devplat connect` used to take 15-17s end to end. Per-phase timing (see
`manager.go`/`firecracker.go`'s `[vmmanager] ... in <duration>` log lines,
and `init.sh`'s `[t=...]` markers, which read `/proc/uptime` since the guest
has no synced wall clock) showed Firecracker itself launching in ~20ms and
the whole guest boot (kernel, cgroup/entropy setup, containerd) landing well
under 2s. The entire bottleneck was inside `dockerd` itself, confirmed
directly in `/var/log/dockerd.log` inside the guest:

```
level=warning msg="Binding to an IP address without --tlsverify is deprecated.
Startup is intentionally being slowed down to show this message"
...
(≈15s gap)
...
level=info msg="Loading containers: start."
```

dockerd deliberately sleeps ~15s when it binds a TCP socket with no
`--tls`/`--tlsverify` flag at all, on the theory that the exposure might be
accidental. Passing `--tls=false` states the intent explicitly and skips
the sleep entirely — this is the fix in `init.sh`'s dockerd invocation.
Real numbers after the fix, three consecutive VMs on the same host:
**2.67s, 2.84s, 2.64s** readiness-wait time.

This fix is baked into `init.sh`, so it's automatically part of any golden
image built after this change. Existing hosts built before it need the
patch applied to their already-built image — see the next section.

## Patching an existing golden image without a full rebuild

`build-golden-image.sh` downloads Alpine and reinstalls every package from
scratch — massive overkill for a one-file change like the `--tls=false`
fix above. `init.sh` is just `/sbin/init` inside the image's ext4
filesystem, so patch it directly via a loop mount instead. (The
Testcontainers port-mapping change is bigger than a one-file patch — it
needs a new guest kernel and a new package in the image; the full
step-by-step host rollout for it is in
`docs/rollout-testcontainers-ports.md`, including a loop-mount variant
that avoids a full rebuild.)

```bash
# 1. Find the image this host's agent actually loads — read it from the
#    .env, don't assume it matches the `current` symlink (see gotcha below).
grep GOLDEN_IMAGE_PATH /opt/devplat/agent/.env
REAL_IMAGE=/opt/devplat/agent/images/<version>/rootfs.ext4   # from above

# 2. Mount it read-write, drop in the updated init.sh, unmount cleanly.
sudo mkdir -p /tmp/golden-patch
sudo mount -o loop,rw "$REAL_IMAGE" /tmp/golden-patch
sudo install -m 0755 image/init.sh /tmp/golden-patch/sbin/init
sudo umount /tmp/golden-patch

# 3. Verify the patch actually landed before trusting it.
sudo mount -o loop,ro "$REAL_IMAGE" /tmp/golden-patch
grep -- "--tls=false" /tmp/golden-patch/sbin/init
sudo umount /tmp/golden-patch

sudo systemctl restart devplat-agent
```

**Gotcha, hit live while rolling this out**: `GOLDEN_IMAGE_PATH` in a
host's `.env` can point at a specific versioned path
(`images/v1/rootfs.ext4`) that *doesn't* match what `images/current` is
symlinked to (`images/v2/rootfs.ext4`, say) — the two can silently drift
apart if a version was ever built without also repointing `.env`. Patching
whichever one `current` resolves to, without checking `.env` first, patches
a file the agent isn't even loading. Always resolve the real path from
`.env` directly. If a host has accumulated multiple version directories
with an unclear relationship between them, compare `images/<v>/MANIFEST`
(`built_at`) and `diff` their `sbin/init` before trusting either — don't
assume the one `current` points at, or the newest `built_at`, is
automatically the good one. (This is also how a bad image switch got
caught here: switching a host to a supposedly-"newer" image broke VM boot
entirely — `GOLDEN_IMAGE_PATH` was rolled back to the known-good version
while the broken one is set aside, unexamined, for later.)

## Known gaps / follow-ups

- **`cgroup setup failed ... permission denied`** on VM boot — root-caused
  and fixed in `cgroup.go`: on cgroup v2, `cpu.max`/`memory.max` only exist
  in a leaf if the parent's `cgroup.subtree_control` enables those
  controllers, which nothing ever did; `os.WriteFile`'s `O_CREATE` against
  the missing file is what kernfs reported as "permission denied".
  `ensureParentControllers` now enables `+cpu +memory` top-down before any
  leaf is written. Verified in this repo's tests only as far as a sandbox
  allows — after deploying, confirm on a real host that the warning is gone
  and `cat /sys/fs/cgroup/devplat/<vm-id>/cpu.max` shows the plan's cap.
  (If a host still runs a hybrid/v1 cgroup layout, the error message now
  says so explicitly instead of "permission denied".)
- **`WARNING: IPv4 forwarding is disabled`** in the guest's dockerd log —
  resolved as a side effect of the port-mapping work: the warning came from
  our own `--ip-forward=false` stopgap flag (part of the broken-iptables
  fallback), not from any host setting. With working guest iptables,
  dockerd runs with stock defaults and enables guest-side forwarding
  itself.
- **No egress allowlist/blocklist** (e.g. against known mining pools) — only
  a bandwidth cap and inbound denial, matching the explicit "first cut"
  scope in the work order.
- **No jailer** (chroot/seccomp hardening) — Firecracker runs directly as
  root for now. Worth revisiting before handling untrusted workloads at
  scale.
- **Crash recovery is partial**: `Manager.reconcile()` restores slot/VM
  bookkeeping and the TTL clock from `meta.json` across an agent restart, so
  capacity accounting and the reaper keep working. It does **not**
  reconnect a live `firecracker-go-sdk` `Machine` handle to an already-running
  process — `Destroy` falls back to `SIGKILL` by PID in that case, which
  works but is coarser than a clean shutdown.
