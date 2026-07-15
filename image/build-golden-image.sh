#!/bin/bash
# Builds one versioned Firecracker golden image: a minimal Alpine rootfs
# with an unmodified Docker daemon + containerd, plus this repo's init.sh
# as PID 1. Run on an x86_64 Linux build host with root (needs loop
# devices, mount, chroot) — NOT inside the microVM, and not inside a
# container without --privileged.
#
# Usage: sudo ./build-golden-image.sh <version> [output_dir]
#   version:    arbitrary tag, e.g. "v1" or a date — becomes the directory
#               name under output_dir, so images are naturally versioned
#               (see devplat-agent/README.md's note on why: no Firecracker
#               snapshots yet, but this keeps that door open later without
#               reshaping anything).
#   output_dir: defaults to /opt/devplat/agent/images
#
# Verify ALPINE_VERSION against https://alpinelinux.org/downloads/ before
# first use — this script was authored without live network access to the
# Alpine CDN to confirm the exact current patch release.
set -euo pipefail

VERSION="${1:?usage: build-golden-image.sh <version> [output_dir]}"
OUTPUT_DIR="${2:-/opt/devplat/agent/images}"

ALPINE_MINOR="${ALPINE_MINOR:-3.20}"
ALPINE_VERSION="${ALPINE_VERSION:-3.20.3}"
ARCH="x86_64"
SIZE_MB="${SIZE_MB:-4096}"

if [ "$(id -u)" -ne 0 ]; then
  echo "must run as root (loop devices, mount, chroot)" >&2
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK_DIR="$(mktemp -d)"
IMAGE_DIR="${OUTPUT_DIR}/${VERSION}"
IMAGE_PATH="${IMAGE_DIR}/rootfs.ext4"
MOUNT_DIR="${WORK_DIR}/mnt"
LOOP_DEV=""

cleanup() {
  set +e
  if mountpoint -q "$MOUNT_DIR" 2>/dev/null; then
    umount "$MOUNT_DIR/dev" 2>/dev/null
    umount "$MOUNT_DIR" 2>/dev/null
  fi
  if [ -n "$LOOP_DEV" ]; then
    losetup -d "$LOOP_DEV" 2>/dev/null
  fi
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

if [ -e "$IMAGE_PATH" ]; then
  echo "refusing to overwrite existing image at $IMAGE_PATH (images are immutable once built — bump the version)" >&2
  exit 1
fi

mkdir -p "$IMAGE_DIR" "$MOUNT_DIR"

echo "==> fetching Alpine minirootfs ${ALPINE_VERSION}"
MINIROOTFS_URL="https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_MINOR}/releases/${ARCH}/alpine-minirootfs-${ALPINE_VERSION}-${ARCH}.tar.gz"
curl -fsSL "$MINIROOTFS_URL" -o "${WORK_DIR}/minirootfs.tar.gz"

echo "==> creating ${SIZE_MB}MB ext4 image"
dd if=/dev/zero of="$IMAGE_PATH" bs=1M count="$SIZE_MB" status=none
mkfs.ext4 -q -F "$IMAGE_PATH"

echo "==> mounting via loopback"
LOOP_DEV="$(losetup -f)"
losetup "$LOOP_DEV" "$IMAGE_PATH"
mount "$LOOP_DEV" "$MOUNT_DIR"

echo "==> extracting rootfs"
tar -xzf "${WORK_DIR}/minirootfs.tar.gz" -C "$MOUNT_DIR"

# apk needs working DNS resolution inside the chroot to reach Alpine's
# package mirrors.
cp /etc/resolv.conf "$MOUNT_DIR/etc/resolv.conf"
mount -t devtmpfs devtmpfs "$MOUNT_DIR/dev"

echo "==> installing docker + containerd inside chroot"
chroot "$MOUNT_DIR" /bin/sh -c '
  set -e
  apk update
  apk add --no-cache docker docker-cli containerd iproute2 ca-certificates
'

echo "==> installing init.sh as PID 1"
install -m 0755 "${SCRIPT_DIR}/init.sh" "$MOUNT_DIR/sbin/init"

echo "==> writing manifest"
cat > "${IMAGE_DIR}/MANIFEST" <<EOF
version=${VERSION}
built_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
alpine_version=${ALPINE_VERSION}
arch=${ARCH}
size_mb=${SIZE_MB}
EOF

umount "$MOUNT_DIR/dev"
umount "$MOUNT_DIR"
losetup -d "$LOOP_DEV"
LOOP_DEV=""

ln -sfn "$IMAGE_DIR" "${OUTPUT_DIR}/current"
echo "==> done: ${IMAGE_PATH}"
echo "    point GOLDEN_IMAGE_PATH at either this path directly, or at"
echo "    ${OUTPUT_DIR}/current/rootfs.ext4 to always track the newest build."
