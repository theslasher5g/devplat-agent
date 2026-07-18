package vmmanager

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// prepareRootfs makes a private, writable copy of the golden image for one
// VM. No snapshotting/COW yet (explicitly out of scope for this build
// step — see devplat repo's build-order notes) — a plain copy keeps the
// storage story trivial: destroying a VM is just deleting its directory,
// and nothing is shared between VMs that a bug could corrupt across runs.
//
// Shells out to cp --sparse=always rather than io.Copy: the golden image is
// a mostly-empty ext4 filesystem (4GB allocated, a fraction of that actually
// written), and a byte-for-byte io.Copy has no idea about the holes — it
// reads and writes all 4GB of zeroes every single boot. On real disk that
// took long enough to blow through the boot-readiness budget further down
// the line (machine.Start() timing out with "context deadline exceeded"
// simply because prepareRootfs had already spent most of the deadline).
// cp's default sparse detection (SEEK_HOLE/SEEK_DATA) skips the holes and
// finishes in well under a second for the same file.
func prepareRootfs(goldenPath, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("create rootfs dir: %w", err)
	}
	out, err := exec.Command("cp", "--sparse=always", goldenPath, destPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("copy golden image %s -> %s: %w (%s)", goldenPath, destPath, err, out)
	}
	return nil
}

func removeVMDir(dir string) error {
	return os.RemoveAll(dir)
}
