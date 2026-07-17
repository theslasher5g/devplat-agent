#!/bin/sh
# Golden image PID 1. Deliberately not a full init system (no OpenRC/systemd)
# — a microVM has exactly one job: run a Docker daemon for the duration of
# one test run, then get destroyed. Kernel boot args set by the agent
# (StaticNetworkConfiguration) already bring up eth0 with a static IP before
# userspace starts, via the kernel's IP autoconfiguration — nothing to do
# here for networking.
set -e

mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev 2>/dev/null || true
mkdir -p /dev/pts && mount -t devpts devpts /dev/pts 2>/dev/null || true

# Whatever stdio the kernel handed PID 1 was silently swallowing every
# echo here (nothing showed up on the Firecracker console log even with
# a checkpoint on the very first line) — force a fresh, explicit fd to
# /dev/console instead of trusting the inherited one. This has to come
# AFTER the mounts above, not before: an earlier attempt put it first
# and got the same silence, because /dev isn't guaranteed populated
# (and /dev/console with it) until the devtmpfs mount above actually
# runs — there's no guarantee the kernel auto-mounted it already.
exec > /dev/console 2>&1
echo "init.sh: starting, /dev/console redirect active"

# The registry pull-through-cache lives on the host, reachable at this VM's
# default gateway (the tap device's host-side IP — see devplat-agent's
# network.go: it's assigned per VM slot, so it can't be baked in statically).
GATEWAY=$(ip route show default | awk '{print $3; exit}')
echo "init.sh: gateway=${GATEWAY:-<none>}"
mkdir -p /etc/docker
if [ -n "$GATEWAY" ]; then
  cat > /etc/docker/daemon.json <<EOF
{
  "registry-mirrors": ["http://${GATEWAY}:5000"]
}
EOF
else
  echo "init.sh: no default gateway found, starting without a registry mirror"
  echo '{}' > /etc/docker/daemon.json
fi

mkdir -p /run/containerd
echo "init.sh: starting containerd"
containerd > /var/log/containerd.log 2>&1 &
echo "init.sh: containerd pid=$!"

# Wait for containerd's socket before handing off to dockerd.
for i in $(seq 1 50); do
  [ -S /run/containerd/containerd.sock ] && break
  sleep 0.1
done
if [ -S /run/containerd/containerd.sock ]; then
  echo "init.sh: containerd socket is up"
else
  echo "init.sh: containerd socket NEVER appeared, dumping its log:"
  cat /var/log/containerd.log 2>&1 || echo "init.sh: (no containerd log found)"
fi

# Unencrypted TCP on 2375 — matches the work order's scope (TLS is a
# follow-up, not required for this build step); only reachable at all
# because the host's iptables DNAT + WireGuard-only binding restrict who
# can reach this port in the first place (see devplat-agent/internal/vmmanager/network.go).
echo "init.sh: exec'ing dockerd"
exec dockerd \
  -H unix:///var/run/docker.sock \
  -H tcp://0.0.0.0:2375 \
  --containerd /run/containerd/containerd.sock \
  --log-level info
