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
- **Egress**: a `tc` token-bucket bandwidth cap per tap device
  (`BANDWIDTH_CAP_MBIT`). No egress allowlist/blocklist yet — flagged as a
  known gap below, matching the work order's "first cut" scope (a bandwidth
  cap + blocking unsolicited inbound is what was asked for).
- **Capacity**: `slots = min(floor(cpu / VM_VCPUS), floor((ram - HOST_RESERVED_RAM_MB) / VM_RAM_MB))`,
  computed from the host's actual detected CPU count and `/proc/meminfo`.
  When registering a host via `devplat-backend`'s `POST /admin/hosts`, use
  this same post-reservation number for `cpuTotal`/`ramTotalMb` — the
  scheduler's own accounting needs to agree with what this agent reports on
  `GET /health`, or the two will drift.

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
   ./image/fetch-kernel.sh /opt/devplat/agent/images/vmlinux.bin
   ```
   Copy `/opt/devplat/agent/images/` to the target host (or build directly
   on it).
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

## Known gaps / follow-ups

- **Golden image build was not run end-to-end here.** The build environment
  this repo was authored in blocks outbound access to Alpine's CDN (a
  sandbox network policy, not a real-world constraint) — `build-golden-image.sh`
  is syntax-checked (`bash -n`) but unexecuted. Verify `ALPINE_VERSION`
  against https://alpinelinux.org/downloads/ before first real use.
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
