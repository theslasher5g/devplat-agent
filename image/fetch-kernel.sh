#!/bin/bash
# Downloads a Firecracker-compatible uncompressed x86_64 kernel (vmlinux).
# This is Firecracker's own official quickstart-guide kernel — confirmed
# reachable (HTTP 200) at authoring time. It's a separate artifact from the
# golden rootfs image (KERNEL_IMAGE_PATH vs. GOLDEN_IMAGE_PATH in the
# agent's env) and rarely needs updating.
#
# Usage: ./fetch-kernel.sh [output_path]
set -euo pipefail

OUTPUT_PATH="${1:-/opt/devplat/agent/images/vmlinux.bin}"
KERNEL_URL="${KERNEL_URL:-https://s3.amazonaws.com/spec.ccfc.min/img/quickstart_guide/x86_64/kernels/vmlinux.bin}"

mkdir -p "$(dirname "$OUTPUT_PATH")"
echo "==> fetching kernel from ${KERNEL_URL}"
curl -fsSL "$KERNEL_URL" -o "$OUTPUT_PATH"
echo "==> done: ${OUTPUT_PATH}"
echo "    point KERNEL_IMAGE_PATH at this file."
