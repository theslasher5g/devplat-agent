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
# t=<seconds since boot, not wall clock — this guest has no RTC/NTP fix at
# this point> prefixes below let us see exactly which phase (cgroup/entropy
# setup, containerd, or dockerd itself) actually eats the several seconds
# between Firecracker launching and the agent's readiness dial succeeding,
# instead of guessing from the outside.
T() { cut -d' ' -f1 /proc/uptime 2>/dev/null || echo '?'; } # '?' before /proc is mounted below
echo "init.sh: starting, /dev/ttyS0 redirect active"

# The kernel starts PID 1 with a near-empty PATH, which is inherited by
# everything we launch — including dockerd, which then can't find `runc`
# or `iptables` (both installed, just not on PATH) and dies with
# "executable file not found in $PATH". Set a real PATH up front.
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

set -e

mount -t proc proc /proc
echo "init.sh: /proc mounted [t=$(T)]"
mount -t sysfs sysfs /sys
echo "init.sh: /sys mounted [t=$(T)]"
mount -t devtmpfs devtmpfs /dev 2>/dev/null || true
mkdir -p /dev/pts && mount -t devpts devpts /dev/pts 2>/dev/null || true
echo "init.sh: /dev ready [t=$(T)]"

# dockerd needs a real cgroup hierarchy to initialize its cgroup driver.
# cgroup2 alone (tried first) didn't fix the silent hang on the original
# 4.14 guest kernel (cgroup v2's cpu controller wasn't merged until 4.15).
# cgroup v1 (per-controller hierarchies) has been stable since 2.6.24 and
# is what dockerd has defaulted to for most of its life. The current 5.10
# guest kernel supports both; v1 is kept because it's the combination that
# has actually been proven in this guest — switching to v2 here would be
# gratuitous churn.
mkdir -p /sys/fs/cgroup
mount -t tmpfs -o mode=755 cgroup_root /sys/fs/cgroup
for ctrl in cpu cpuacct memory blkio devices freezer pids net_cls net_prio perf_event; do
  mkdir -p "/sys/fs/cgroup/$ctrl"
  mount -t cgroup -o "$ctrl" cgroup "/sys/fs/cgroup/$ctrl" 2>/dev/null || rmdir "/sys/fs/cgroup/$ctrl" 2>/dev/null
done
echo "init.sh: cgroup v1 hierarchies mounted: $(ls /sys/fs/cgroup) [t=$(T)]"

# /etc/resolv.conf baked into the golden image is a leftover from the BUILD
# host (build-golden-image.sh copies its /etc/resolv.conf into the chroot so
# `apk` can resolve packages) — it points at 127.0.0.53, the build host's own
# systemd-resolved stub. This guest doesn't run systemd-resolved at all, so
# dockerd's own image pulls failed DNS resolution with "connection refused"
# against a resolver that was never actually reachable from in here. Force
# a real, always-reachable resolver regardless of what got baked in.
echo 'nameserver 1.1.1.1' > /etc/resolv.conf
echo "init.sh: /etc/resolv.conf -> $(cat /etc/resolv.conf)"

# Seed the kernel entropy pool immediately. THIS is the fix for the bug that
# had every boot look like a silent hang: this guest has no virtio-rng device
# and none of the interrupt sources (disk, input, network noise) a real
# machine uses to seed randomness, so the kernel CRNG took ~80s to initialize
# from scratch. dockerd's Go runtime calls getrandom(), which BLOCKS until the
# CRNG is ready — so dockerd printed nothing and never opened its port within
# any reasonable readiness window. On the current 5.10 guest kernel the
# random.trust_cpu=on kernel arg (already passed by the agent, and honored
# since 4.19) fixes this on its own; haveged stays as belt-and-braces so a
# host without RDRAND, or a future kernel swap, can't silently reintroduce
# an 80-second boot stall.
haveged -w 1024 2>/dev/null && echo "init.sh: haveged started [t=$(T)]" || echo "init.sh: WARNING haveged failed to start [t=$(T)]"
echo "init.sh: entropy_avail=$(cat /proc/sys/kernel/random/entropy_avail 2>/dev/null || echo '?') [t=$(T)]"

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
echo "init.sh: starting containerd [t=$(T)]"
containerd > /var/log/containerd.log 2>&1 &
echo "init.sh: containerd pid=$! [t=$(T)]"

# Wait for containerd's socket before handing off to dockerd.
for i in $(seq 1 50); do
  [ -S /run/containerd/containerd.sock ] && break
  sleep 0.1
done
if [ -S /run/containerd/containerd.sock ]; then
  echo "init.sh: containerd socket is up [t=$(T)]"
else
  echo "init.sh: containerd socket NEVER appeared after ~5s [t=$(T)], dumping its log:"
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
# Pick an iptables backend the running kernel actually supports. Alpine's
# default iptables is the nft backend, which needs CONFIG_NF_TABLES; the
# Firecracker CI 5.10 guest kernel (see fetch-kernel.sh) ships legacy
# x_tables only, so on it the nft backend fails with "Protocol not
# supported". The image installs Alpine's iptables-legacy package precisely
# for this: probe the default backend first, and only if it can't talk to
# the kernel, repoint every iptables command at the legacy binaries. On a
# future kernel with nf_tables enabled the probe succeeds and the nft
# backend stays — nothing to change here.
if ! iptables -t nat -L >/dev/null 2>&1; then
  for cmd in iptables iptables-save iptables-restore ip6tables ip6tables-save ip6tables-restore; do
    base="${cmd%%-*}"                 # iptables / ip6tables
    suffix="${cmd#"$base"}"           # "" / -save / -restore
    legacy="${base}-legacy${suffix}"  # iptables-legacy / iptables-legacy-save / ...
    lp="$(command -v "$legacy" 2>/dev/null)" || continue
    cp="$(command -v "$cmd" 2>/dev/null)" || continue
    ln -sf "$lp" "$cp"
  done
fi

# Decide whether dockerd can manage iptables at all. If neither the nft nor
# the legacy backend works on this kernel, fall back to --iptables=false so
# the daemon still comes up — `docker version` and image pulls work, but
# container port publishing doesn't (this was the permanent state on the old
# 4.14 kernel; on the 5.10 kernel it's strictly an emergency fallback).
# When iptables works, dockerd runs with stock networking defaults:
# --iptables=true so it builds its own DOCKER/DOCKER-USER NAT chains,
# --ip-forward=true so it enables net.ipv4.ip_forward itself (this is the
# guest-side forwarding the old setup's "WARNING: IPv4 forwarding is
# disabled" complained about — that warning was caused by our explicit
# --ip-forward=false stopgap, not by anything on the host), and the default
# --userland-proxy=true: docker-proxy processes per published port are the
# most battle-tested path, and unlike hairpin-NAT mode they don't depend on
# route_localnet sysctls for guest-local access to published ports. Traffic
# arriving on eth0 (the agent's per-port proxy dialing this VM) hits the
# DOCKER DNAT chain either way, so the choice only affects edge cases —
# take the default.
if iptables -t nat -L >/dev/null 2>&1; then
  echo "init.sh: iptables works -> $(iptables --version 2>&1 | head -1); dockerd will manage NAT"
  IPTABLES_FLAGS=""
else
  echo "init.sh: WARNING iptables non-functional on this kernel; starting dockerd with --iptables=false"
  IPTABLES_FLAGS="--iptables=false --ip-forward=false --bridge=none"
fi

echo "init.sh: starting dockerd [t=$(T)]"
# --tls=false is not about disabling security here (TLS was never on; this
# port is only reachable through the host's DNAT + WireGuard tunnel — see
# network.go) — it's what stops dockerd's own ~15s self-imposed startup
# delay. Binding -H tcp://... without ANY explicit --tls/--tlsverify flag
# makes dockerd assume the exposure might be accidental and deliberately
# sleep ~15s so the operator notices its "insecure API" warning
# (confirmed directly in dockerd.log: "Startup is intentionally being
# slowed down to show this message"). Passing --tls=false states the
# exposure is intentional, which skips that sleep entirely — this was the
# entire connect-latency bottleneck, not VM boot or containerd.
dockerd \
  -H unix:///var/run/docker.sock \
  -H tcp://0.0.0.0:2375 \
  --tls=false \
  --containerd /run/containerd/containerd.sock \
  $IPTABLES_FLAGS \
  --log-level info > /var/log/dockerd.log 2>&1 &
DOCKERD_PID=$!
echo "init.sh: dockerd pid=$DOCKERD_PID [t=$(T)]"

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
  echo "init.sh: dockerd is listening on 2375 [t=$(T)]"
else
  echo "init.sh: dockerd NOT listening after ~20s (or it exited) [t=$(T)]"
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
