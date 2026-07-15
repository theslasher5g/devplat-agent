package vmmanager

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

const cgroupRoot = "/sys/fs/cgroup/devplat"

// createCgroup makes a cgroup v2 leaf for one VM and caps its CPU/RAM. The
// Firecracker process (and everything it forks) is added to it after boot —
// see firecracker.go — so the whole microVM, not just the guest kernel, is
// bounded regardless of what runs inside.
func createCgroup(vmID string, vcpus int64, ramMb int64) (string, error) {
	path := filepath.Join(cgroupRoot, vmID)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", fmt.Errorf("create cgroup dir: %w", err)
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
