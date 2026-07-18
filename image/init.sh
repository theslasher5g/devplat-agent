#!/bin/sh
# Golden image PID 1. Deliberately not a full init system (no OpenRC/systemd)
# — a microVM has exactly one job: run a Docker daemon for the duration of
# one test run, then get destroyed. Kernel boot args set by the agent
# (StaticNetworkConfiguration) already bring up eth0 with a static IP before
# userspace starts, via the kernel's IP autoconfiguration — nothing to do
# here for networking.
#
# Two earlier attempts at redirecting to /dev/console (both before and
# after this script's own devtmpfs mount) produced total silence — every
# kernel printk shows up fine on this console, so ttyS0 itself works;
# /dev/console apparently isn't reliably aliased to it in this guest.
# Target /dev/ttyS0 directly instead: it's the exact device named in the
# kernel command line (console=ttyS0) that's already proven to work,
# and the kernel's own automatic devtmpfs mount (visible in dmesg before
# userspace even starts) means /dev is already populated at this point —
# no mount of our own needed first.
exec > /dev/ttyS0 2>&1
echo "init.sh: starting, /dev/ttyS0 redirect active"

set -e

mount -t proc proc /proc
echo "init.sh: /proc mounted"
mount -t sysfs sysfs /sys
echo "init.sh: /sys mounted"
mount -t devtmpfs devtmpfs /dev 2>/dev/null || true
mkdir -p /dev/pts && mount -t devpts devpts /dev/pts 2>/dev/null || true
echo "init.sh: /dev ready"

# dockerd needs a real cgroup hierarchy to initialize its cgroup driver —
# without this, /sys/fs/cgroup is just an empty directory under the plain
# sysfs mount above, and dockerd was observed hanging silently past the
# 20s readiness window (confirmed working fine via chroot on the host,
# which inherits the host's own working cgroup mount — the guest never had
# one at all).
mkdir -p /sys/fs/cgroup
mount -t cgroup2 cgroup2 /sys/fs/cgroup 2>/dev/null || true
echo "init.sh: cgroup2 mounted at /sys/fs/cgroup"

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
