#!/usr/bin/env bash
set -euo pipefail

VERSION="${1:-dev}"
ARCH="${2:-$(uname -m)}"

# Normalize arch naming
case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
esac

DIST_DIR="dist"
mkdir -p "$DIST_DIR"

echo "==> Building OCI image (flowcraft/runtime:${VERSION})"
docker build -t "flowcraft/runtime:${VERSION}" -t "flowcraft/runtime:latest" .

echo "==> Exporting rootfs tarball for WSL"
CONTAINER_ID=$(docker create "flowcraft/runtime:${VERSION}")
docker export "$CONTAINER_ID" | gzip > "${DIST_DIR}/flowcraft-wsl-${VERSION}-amd64.tar.gz"
docker rm "$CONTAINER_ID" > /dev/null

echo "==> Converting to qcow2 for macOS VM"
if command -v qemu-img &> /dev/null; then
  # Create a raw disk, copy rootfs into it, convert to qcow2
  RAW_DISK="${DIST_DIR}/flowcraft-vm-${VERSION}-${ARCH}.raw"
  QCOW2="${DIST_DIR}/flowcraft-vm-${VERSION}-${ARCH}.qcow2"

  truncate -s 4G "$RAW_DISK"
  mkfs.ext4 -q "$RAW_DISK"

  MOUNT_DIR=$(mktemp -d)
  sudo mount -o loop "$RAW_DISK" "$MOUNT_DIR"
  docker export "$CONTAINER_ID" 2>/dev/null | sudo tar -xf - -C "$MOUNT_DIR" || \
    sudo tar -xzf "${DIST_DIR}/flowcraft-wsl-${VERSION}-amd64.tar.gz" -C "$MOUNT_DIR"
  sudo umount "$MOUNT_DIR"
  rmdir "$MOUNT_DIR"

  qemu-img convert -f raw -O qcow2 "$RAW_DISK" "$QCOW2"
  rm "$RAW_DISK"
  echo "  -> ${QCOW2}"
else
  echo "  -> qemu-img not found; skipping qcow2 conversion"
fi

echo "==> Build artifacts:"
ls -lh "${DIST_DIR}/"

echo "Done."
