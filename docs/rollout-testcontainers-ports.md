# Rollout: real Testcontainers port mapping

This is the host-side runbook for the change that makes container-published
ports (`-p` / Testcontainers mapped ports) actually reachable from client
machines. Everything here is what `git pull` does NOT do for you. Code
changes involved (all on the same branch, one per repo):

- **devplat-agent**: new guest kernel + `iptables-legacy` in the golden
  image, `init.sh` runs dockerd with real NAT again, new
  `GET /vms/{id}/proxy/{port}` endpoint, cgroup v2 fix.
- **devplat-backend**: new `/environments/:id/tunnel/:port` WebSocket route.
- **devplat-cli**: mirrors published container ports onto the same local
  ports (new binaries must be released).
- **devplat-frontend**: docs no longer list port mapping as a known gap —
  deploy this LAST, only after the live verification below passes.

Order matters: **agent first** (per host), **backend second**, **CLI
release third**, **frontend last**. Old CLIs keep working throughout (they
just don't mirror ports). Do one host (pve01), verify, then repeat on
pve02.

## 1. Per data-plane host (pve01 first, then pve02)

### 1a. Pull + rebuild the agent binary

```bash
cd <checkout of devplat-agent on the host>
git pull
go build -o /usr/local/bin/devplat-agent ./cmd/agent
```

(Don't restart the service yet — do image + kernel + .env first, one
restart at the end.)

### 1b. Fetch the new guest kernel

```bash
sudo ./image/fetch-kernel.sh /opt/devplat/agent/images/vmlinux-5.10.239.bin
```

This is the Firecracker project's own CI guest kernel 5.10.239. Its
published config was verified to contain everything Docker's bridge
networking needs (bridge, veth, overlayfs, conntrack/NAT, legacy x_tables,
`CONFIG_IP_PNP` for the kernel-arg static IP the agent already uses).
Leave the old `vmlinux.bin` in place — rollback is then a one-line `.env`
change.

### 1c. New golden image

Preferred: full rebuild (new package `iptables-legacy` is baked in
properly, MANIFEST stays truthful):

```bash
sudo ./image/build-golden-image.sh v3 /opt/devplat/agent/images
```

(Choose the next free version number; check `ls /opt/devplat/agent/images`.)

Faster loop-mount variant if you want to avoid the full rebuild — patches
the package and init.sh into a COPY of the image the host actually loads:

```bash
grep GOLDEN_IMAGE_PATH /opt/devplat/agent/.env     # resolve the REAL image (do not trust `current`)
REAL=/opt/devplat/agent/images/<version>/rootfs.ext4
sudo mkdir -p /opt/devplat/agent/images/v3
sudo cp --sparse=always "$REAL" /opt/devplat/agent/images/v3/rootfs.ext4

sudo mkdir -p /tmp/golden-patch
sudo mount -o loop,rw /opt/devplat/agent/images/v3/rootfs.ext4 /tmp/golden-patch
# apk inside the chroot needs DNS; the baked-in resolv.conf is a build-host stub
echo 'nameserver 1.1.1.1' | sudo tee /tmp/golden-patch/etc/resolv.conf >/dev/null
sudo chroot /tmp/golden-patch apk add --no-cache iptables-legacy
sudo install -m 0755 image/init.sh /tmp/golden-patch/sbin/init
# verify before trusting it:
sudo chroot /tmp/golden-patch /sbin/iptables-legacy --version   # iptables v1.8.10 (legacy)
grep -c 'IPTABLES_FLAGS=""' /tmp/golden-patch/sbin/init          # 1
sudo umount /tmp/golden-patch
```

Either way you end up with a NEW versioned directory — never overwrite the
image a running host is loading.

### 1d. Point `.env` at both new artifacts

Edit `/opt/devplat/agent/.env`:

```
KERNEL_IMAGE_PATH=/opt/devplat/agent/images/vmlinux-5.10.239.bin
GOLDEN_IMAGE_PATH=/opt/devplat/agent/images/v3/rootfs.ext4
```

Set both explicitly (remember the documented drift gotcha: `current` and
`.env` can disagree — `.env` is what counts).

### 1e. Restart + verify on this host

```bash
sudo systemctl restart devplat-agent
journalctl -u devplat-agent -f &
```

Boot one VM (easiest: `devplat connect` from any machine, or a direct
`POST /vms` with the agent token over WireGuard), then check, in order:

1. **Guest console** (`/var/lib/devplat/vms/<vm_id>/console.log`):
   - `init.sh: iptables works -> iptables v1.8.10 (legacy); dockerd will manage NAT`
     — if you instead see `WARNING iptables non-functional`, the image or
     kernel didn't actually change; re-check `.env` and the MANIFEST.
   - the `iptables-save` dump at the end shows Docker's own chains
     (`DOCKER`, `DOCKER-USER`, nat table entries) — this IS the
     "`iptables -t nat -L` works in the guest" acceptance check, captured
     from inside the guest.
   - no `WARNING: IPv4 forwarding is disabled` in the dockerd.log dump.
2. **cgroup fix**: agent journal has no `cgroup setup failed` warning, and
   `cat /sys/fs/cgroup/devplat/<vm_id>/cpu.max` shows e.g. `200000 100000`
   (2 vCPU plan) while the VM runs. If it errors with the new "not a
   cgroup v2 (unified) hierarchy" message instead, the host is on
   hybrid/v1 cgroups — note it and move on (non-blocking, the VM still
   runs; fixing it means a host reboot with
   `systemd.unified_cgroup_hierarchy=1`).
3. **Port publishing end-to-end on the host** (no client needed): with the
   VM's guest IP from the agent log (`fc-tap-<slot>` → `.2` address):
   ```bash
   DOCKER_HOST=tcp://<guest-ip>:2375 docker run -d -p 5432:5432 -e POSTGRES_PASSWORD=x postgres:16-alpine
   DOCKER_HOST=tcp://<guest-ip>:2375 docker ps   # 0.0.0.0:5432->5432/tcp
   nc -vz <guest-ip> 5432                        # succeeded = guest-side DNAT works
   ```

Rollback for this host = revert the two `.env` lines to the old kernel/
image and `systemctl restart devplat-agent`. The old agent binary is only
needed if the new one misbehaves (`git checkout <old sha> && go build ...`).

## 2. Control plane (VPS)

No schema migration, no new env vars, no WireGuard/routing changes — the
per-port tunnel reaches guests through the agent's existing WireGuard
endpoint.

```bash
cd /opt/devplat/backend && git pull
cd /opt/devplat && docker compose build api && docker compose up -d api
```

Quick check: `docker compose logs api --tail 20` shows a clean start; the
new route 404s/upgrades only with auth, so a plain
`curl https://api.devplat.ch/environments/x/tunnel/80` returning 401 is
the expected signature that it's live.

## 3. CLI release

```bash
cd <devplat-cli checkout> && git pull
./scripts/build-release.sh v<next>
# then publish dist/<version>/ the same way as previous releases (get.devplat.ch)
```

## 4. Live verification (the actual acceptance test)

From a real client machine with the NEW CLI:

```bash
devplat connect
# inside the session:
docker run -d --name pg -p 5432 -e POSTGRES_PASSWORD=x postgres:16-alpine
docker port pg        # e.g. 5432/tcp -> 0.0.0.0:32768
# from another local terminal:
psql "host=127.0.0.1 port=32768 user=postgres password=x" -c 'select 1;'
```

Then the real thing — a Java Testcontainers project (Postgres or Kafka)
via `mvn verify` inside the `devplat connect` session, where the test
talks to the container through its mapped port. Only after that passes:
repeat section 1 on pve02, then deploy the frontend (its docs now describe
port mirroring as shipped, which must not go live before it's true).

## Notes / limitations

- Only TCP ports are mirrored (the tunnel is TCP; UDP isn't relayed).
- If a mapped port is already taken on the client machine, the CLI prints
  a one-line warning to stderr and retries; the usual fix is to close
  whatever holds the port locally.
- The CLI polls `containers/json` every 250ms over one keep-alive tunnel
  connection — a port becomes reachable locally within ~a quarter second
  of the container starting, which is inside every Testcontainers wait
  strategy's tolerance.
