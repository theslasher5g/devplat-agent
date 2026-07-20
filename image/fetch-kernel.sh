#!/bin/bash
# Downloads a Firecracker-compatible uncompressed x86_64 kernel (vmlinux).
#
# Default is the Firecracker project's own CI guest kernel 5.10.239 (their
# v1.13 CI artifact set) — NOT the old quickstart-guide 4.14 kernel this
# used to fetch. The 4.14 kernel cannot support container networking inside
# the guest: Alpine's iptables (nft backend) needs nf_tables, which 4.14
# predates, and that left dockerd running with --iptables=false/--bridge=none
# (no port publishing at all). The 5.10.239 CI kernel's published .config
# (same URL + ".config") was verified to include everything Docker's bridge
# networking needs: IP_NF_IPTABLES/IP_NF_NAT/IP_NF_TARGET_MASQUERADE,
# xt_addrtype/xt_conntrack/xt_nat, BRIDGE, BRIDGE_NETFILTER, VETH,
# OVERLAY_FS, all namespaces, MEMCG, CFS_BANDWIDTH — all built-in (=y; these
# guest kernels have no module loading, so built-in is required).
#
# NOTE: this kernel ships LEGACY x_tables only — CONFIG_NF_TABLES is off in
# the published artifact (the repo's main-branch config enables it, but no
# released CI binary has it yet). That's why the golden image installs
# Alpine's iptables-legacy package and init.sh repoints iptables at it when
# the nft backend can't talk to the kernel. If you ever swap in a kernel
# with nf_tables enabled, init.sh detects that and keeps the nft backend —
# nothing else needs changing.
#
# It's a separate artifact from the golden rootfs image (KERNEL_IMAGE_PATH
# vs. GOLDEN_IMAGE_PATH in the agent's env). Keep the old vmlinux.bin around
# and fetch this to a NEW file, so rollback is a one-line .env change.
#
# Usage: ./fetch-kernel.sh [output_path]
set -euo pipefail

OUTPUT_PATH="${1:-/opt/devplat/agent/images/vmlinux-5.10.239.bin}"
KERNEL_URL="${KERNEL_URL:-https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.13/x86_64/vmlinux-5.10.239}"

mkdir -p "$(dirname "$OUTPUT_PATH")"
echo "==> fetching kernel from ${KERNEL_URL}"
curl -fsSL "$KERNEL_URL" -o "$OUTPUT_PATH"
echo "==> done: ${OUTPUT_PATH}"
echo "    point KERNEL_IMAGE_PATH at this file."
