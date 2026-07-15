package vmmanager

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// prepareRootfs makes a private, writable copy of the golden image for one
// VM. No snapshotting/COW yet (explicitly out of scope for this build
// step — see devplat repo's build-order notes) — a plain copy keeps the
// storage story trivial: destroying a VM is just deleting its directory,
// and nothing is shared between VMs that a bug could corrupt across runs.
func prepareRootfs(goldenPath, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("create rootfs dir: %w", err)
	}
	src, err := os.Open(goldenPath)
	if err != nil {
		return fmt.Errorf("open golden image %s: %w", goldenPath, err)
	}
	defer src.Close()

	dst, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create rootfs copy %s: %w", destPath, err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("copy golden image: %w", err)
	}
	return nil
}

func removeVMDir(dir string) error {
	return os.RemoveAll(dir)
}
