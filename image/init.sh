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

# The registry pull-through-cache lives on the host, reachable at this VM's
# default gateway (the tap device's host-side IP — see devplat-agent's
# network.go: it's assigned per VM slot, so it can't be baked in statically).
GATEWAY=$(ip route show default | awk '{print $3; exit}')
mkdir -p /etc/docker
if [ -n "$GATEWAY" ]; then
  cat > /etc/docker/daemon.json <<EOF
{
  "registry-mirrors": ["http://${GATEWAY}:5000"]
}
EOF
else
  echo "init.sh: no default gateway found, starting without a registry mirror" >&2
  echo '{}' > /etc/docker/daemon.json
fi

mkdir -p /run/containerd
containerd > /var/log/containerd.log 2>&1 &

# Wait for containerd's socket before handing off to dockerd.
for i in $(seq 1 50); do
  [ -S /run/containerd/containerd.sock ] && break
  sleep 0.1
done

# Unencrypted TCP on 2375 — matches the work order's scope (TLS is a
# follow-up, not required for this build step); only reachable at all
# because the host's iptables DNAT + WireGuard-only binding restrict who
# can reach this port in the first place (see devplat-agent/internal/vmmanager/network.go).
exec dockerd \
  -H unix:///var/run/docker.sock \
  -H tcp://0.0.0.0:2375 \
  --containerd /run/containerd/containerd.sock \
  --log-level warn
