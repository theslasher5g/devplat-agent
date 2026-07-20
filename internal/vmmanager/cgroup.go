package vmmanager

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const cgroupMount = "/sys/fs/cgroup"
const cgroupRoot = cgroupMount + "/devplat"

// ensureParentControllers makes the cpu and memory controllers usable in
// leaves under cgroupRoot. This was the missing step behind the
// "cgroup setup failed ... permission denied" warning on every VM boot: on
// cgroup v2, a controller's interface files (cpu.max, memory.max) only
// exist in a child cgroup if that controller is enabled in the PARENT's
// cgroup.subtree_control. createCgroup never enabled anything, so the
// fresh leaf had no cpu.max at all — and os.WriteFile's O_CREATE on a
// cgroupfs directory is refused by the kernel with a permission error
// (kernfs doesn't allow creating files), which is exactly the misleading
// "permission denied" that was logged. Enable the controllers top-down
// (root, then devplat) before touching any leaf; re-enabling an
// already-enabled controller is a no-op, so this is safe to call per VM.
func ensureParentControllers() error {
	if _, err := os.Stat(filepath.Join(cgroupMount, "cgroup.controllers")); err != nil {
		return fmt.Errorf("%s is not a cgroup v2 (unified) hierarchy — per-VM resource caps need one (on a hybrid/v1 host, boot with systemd.unified_cgroup_hierarchy=1): %w", cgroupMount, err)
	}
	if err := os.MkdirAll(cgroupRoot, 0o755); err != nil {
		return fmt.Errorf("create cgroup dir %s: %w", cgroupRoot, err)
	}
	for _, dir := range []string{cgroupMount, cgroupRoot} {
		p := filepath.Join(dir, "cgroup.subtree_control")
		// One controller per write: a combined "+cpu +memory" write fails
		// atomically, which would hide WHICH controller the kernel refused.
		for _, ctrl := range []string{"+cpu", "+memory"} {
			if err := os.WriteFile(p, []byte(ctrl), 0o644); err != nil {
				return fmt.Errorf("enable %s in %s: %w", ctrl, p, err)
			}
		}
	}
	return nil
}

// createCgroup makes a cgroup v2 leaf for one VM and caps its CPU/RAM. The
// Firecracker process (and everything it forks) is added to it after boot —
// see firecracker.go — so the whole microVM, not just the guest kernel, is
// bounded regardless of what runs inside.
func createCgroup(vmID string, vcpus int64, ramMb int64) (string, error) {
	if err := ensureParentControllers(); err != nil {
		return "", err
	}
	path := filepath.Join(cgroupRoot, vmID)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", fmt.Errorf("create cgroup dir: %w", err)
	}
	// Sanity-check the controllers actually reached this leaf before writing
	// limit files — a clear error beats kernfs's opaque one.
	if ctrls, err := os.ReadFile(filepath.Join(path, "cgroup.controllers")); err == nil {
		have := string(ctrls)
		for _, want := range []string{"cpu", "memory"} {
			if !strings.Contains(have, want) {
				return path, fmt.Errorf("controller %q not available in %s (have: %q)", want, path, strings.TrimSpace(have))
			}
		}
	}
	// cpu.max format: "<quota> <period>", both in microseconds — N vcpus of
	// a 100ms period is N*100000 quota.
	cpuMax := fmt.Sprintf("%d 100000", vcpus*100000)
	if err := os.WriteFile(filepath.Join(path, "cpu.max"), []byte(cpuMax), 0o644); err != nil {
		return path, fmt.Errorf("set cpu.max: %w", err)
	}
	memMax := strconv.FormatInt(ramMb*1024*1024, 10)
	if err := os.WriteFile(filepath.Join(path, "memory.max"), []byte(memMax), 0o644); err != nil {
		return path, fmt.Errorf("set memory.max: %w", err)
	}
	return path, nil
}

func addProcessToCgroup(cgroupPath string, pid int) error {
	return os.WriteFile(filepath.Join(cgroupPath, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644)
}

func removeCgroup(vmID string) error {
	err := os.Remove(filepath.Join(cgroupRoot, vmID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
