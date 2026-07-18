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

# dockerd needs a real cgroup hierarchy to initialize its cgroup driver.
# cgroup2 alone (tried first) didn't fix the silent hang — this guest
# kernel is Firecracker's stock 4.14.174, and the cgroup v2 *cpu*
# controller wasn't merged until 4.15; mounting cgroup2 here gives dockerd
# a filesystem but not the controller files a modern dockerd's cgroupfs
# driver expects, which plausibly explains a hang rather than a clean
# error. cgroup v1 (per-controller hierarchies) has been stable since
# 2.6.24 and is what dockerd has defaulted to for most of its life — far
# safer bet on a kernel this old.
mkdir -p /sys/fs/cgroup
mount -t tmpfs -o mode=755 cgroup_root /sys/fs/cgroup
for ctrl in cpu cpuacct memory blkio devices freezer pids net_cls net_prio perf_event; do
  mkdir -p "/sys/fs/cgroup/$ctrl"
  mount -t cgroup -o "$ctrl" cgroup "/sys/fs/cgroup/$ctrl" 2>/dev/null || rmdir "/sys/fs/cgroup/$ctrl" 2>/dev/null
done
echo "init.sh: cgroup v1 hierarchies mounted: $(ls /sys/fs/cgroup)"

# Seed the kernel entropy pool immediately. THIS is the fix for the bug that
# had every boot look like a silent hang: this guest has no virtio-rng device
# and none of the interrupt sources (disk, input, network noise) a real
# machine uses to seed randomness, so the kernel CRNG took ~80s to initialize
# from scratch. dockerd's Go runtime calls getrandom(), which BLOCKS until the
# CRNG is ready — so dockerd printed nothing and never opened its port within
# any reasonable readiness window. (The random.trust_cpu=on kernel arg would
# also solve this, but only on Linux 4.19+; this guest kernel is 4.14, which
# predates that option and silently ignores it.) haveged fills the pool from
# CPU timing jitter within a second or two, kernel-version-independently.
haveged -w 1024 2>/dev/null && echo "init.sh: haveged started" || echo "init.sh: WARNING haveged failed to start"
echo "init.sh: entropy_avail=$(cat /proc/sys/kernel/random/entropy_avail 2>/dev/null || echo '?')"

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
#
# Backgrounded with its own log file rather than exec'd directly onto the
# console: dockerd was observed producing zero bytes of output on the live
# ttyS0 console every time it was exec'd here, despite the identical binary
# logging immediately when run via chroot on the host — capturing to a file
# and dumping it on a failed readiness check sidesteps whatever is eating
# the live console output and actually shows the failure. `wait` at the end
# keeps this script (PID 1) alive exactly as long as dockerd runs, same as
# exec'ing it directly would have.
echo "init.sh: starting dockerd"
dockerd \
  -H unix:///var/run/docker.sock \
  -H tcp://0.0.0.0:2375 \
  --containerd /run/containerd/containerd.sock \
  --userland-proxy=false \
  --log-level info > /var/log/dockerd.log 2>&1 &
DOCKERD_PID=$!
echo "init.sh: dockerd pid=$DOCKERD_PID"

# Read /proc/net/tcp directly to detect the listen socket (busybox nc's -z
# doesn't behave like GNU nc's). 2375 decimal = 0947 hex; state 0A = LISTEN.
# Loop caps at ~20s so that on failure this diagnostic dump runs BEFORE the
# agent's own 30s readiness timeout kills the VM out from under us.
for i in $(seq 1 100); do
  grep -q ' 00000000:0947 .* 0A ' /proc/net/tcp 2>/dev/null && break
  kill -0 "$DOCKERD_PID" 2>/dev/null || break
  sleep 0.2
done
if grep -q ' 00000000:0947 .* 0A ' /proc/net/tcp 2>/dev/null; then
  echo "init.sh: dockerd is listening on 2375"
else
  echo "init.sh: dockerd NOT listening after ~20s (or it exited)"
fi
# Always dump the daemon log + full network state, whether or not the socket
# came up: the agent reaches this guest by dialing eth0's IP directly over the
# tap link, so if dockerd's own iptables/bridge/ip_forward setup disturbs eth0
# (a plausible cause of the host seeing i/o timeout even while dockerd is
# listening inside), it'll show here.
echo "=== init.sh: dockerd.log ==="
cat /var/log/dockerd.log 2>&1 || echo "(no dockerd log)"
echo "=== init.sh: ip addr ==="
ip addr 2>&1
echo "=== init.sh: ip route ==="
ip route 2>&1
echo "=== init.sh: iptables-save ==="
iptables-save 2>&1 || echo "(iptables-save unavailable)"
echo "=== init.sh: listening sockets (/proc/net/tcp) ==="
cat /proc/net/tcp 2>&1
echo "=== init.sh: end diagnostics ==="

wait "$DOCKERD_PID"
