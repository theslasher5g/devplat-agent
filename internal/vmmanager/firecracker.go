package vmmanager

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/theslasher5g/devplat-agent/internal/config"
)

func ptrString(v string) *string { return &v }
func ptrBool(v bool) *bool       { return &v }
func ptrInt64(v int64) *int64    { return &v }

// FirecrackerBackend is the production Backend: real Firecracker microVMs
// over the SDK, real tap devices, real cgroups. Not exercised by any test in
// this repo — it needs /dev/kvm, root, and the golden image, none of which
// exist in a CI/dev sandbox. manager_test.go covers the slot/capacity logic
// against a fake Backend instead; this file should be exercised by the
// acceptance checklist on real hardware (Host A/B).
type FirecrackerBackend struct {
	cfg config.Config

	mu       sync.Mutex
	machines map[string]*firecracker.Machine // live handles; empty after an agent restart
}

func NewFirecrackerBackend(cfg config.Config) *FirecrackerBackend {
	return &FirecrackerBackend{cfg: cfg, machines: map[string]*firecracker.Machine{}}
}

func (b *FirecrackerBackend) Boot(ctx context.Context, vm *VM, nc NetConfig, rootfsPath string) error {
	if err := setupTapDevice(nc); err != nil {
		return fmt.Errorf("tap setup: %w", err)
	}
	if err := setupFirewall(b.cfg, nc); err != nil {
		_ = teardownTapDevice(nc)
		return fmt.Errorf("firewall setup: %w", err)
	}
	if err := setupBandwidthCap(nc, b.cfg.BandwidthCapMbit); err != nil {
		_ = teardownFirewall(b.cfg, nc)
		_ = teardownTapDevice(nc)
		return fmt.Errorf("bandwidth cap: %w", err)
	}

	socketPath := filepath.Join(b.cfg.VMStateDir, vm.ID, "firecracker.sock")
	logPath := filepath.Join(b.cfg.VMStateDir, vm.ID, "firecracker.log")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return fmt.Errorf("create vm state dir: %w", err)
	}

	fcCfg := firecracker.Config{
		SocketPath:      socketPath,
		LogPath:         logPath,
		LogLevel:        "Warning",
		KernelImagePath: b.cfg.KernelImagePath,
		// console on ttyS0 for boot-failure debugging via the log; reboot=k
		// makes a guest kernel panic kill the VM instead of looping, so the
		// reaper doesn't have to distinguish "slow" from "stuck".
		KernelArgs: "console=ttyS0 reboot=k panic=1 pci=off",
		Drives: []models.Drive{{
			DriveID:      ptrString("rootfs"),
			PathOnHost:   ptrString(rootfsPath),
			IsRootDevice: ptrBool(true),
			IsReadOnly:   ptrBool(false),
		}},
		NetworkInterfaces: firecracker.NetworkInterfaces{{
			StaticConfiguration: &firecracker.StaticNetworkConfiguration{
				HostDevName: nc.TapName,
				IPConfiguration: &firecracker.IPConfiguration{
					IPAddr:      net.IPNet{IP: nc.GuestIP, Mask: nc.Mask},
					Gateway:     nc.HostIP,
					Nameservers: []string{"1.1.1.1"},
				},
			},
		}},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  ptrInt64(vm.Vcpu),
			MemSizeMib: ptrInt64(vm.RamMb),
		},
		VMID: vm.ID,
	}

	cmd := firecracker.VMCommandBuilder{}.
		WithBin(b.cfg.FirecrackerBinary).
		WithSocketPath(socketPath).
		Build(ctx)

	machine, err := firecracker.NewMachine(ctx, fcCfg, firecracker.WithProcessRunner(cmd))
	if err != nil {
		_ = teardownFirewall(b.cfg, nc)
		_ = teardownTapDevice(nc)
		return fmt.Errorf("configure machine: %w", err)
	}
	if err := machine.Start(ctx); err != nil {
		_ = teardownFirewall(b.cfg, nc)
		_ = teardownTapDevice(nc)
		return fmt.Errorf("start machine: %w", err)
	}

	pid, err := machine.PID()
	if err != nil {
		fmt.Printf("[vmmanager] warning: could not get PID for %s, cgroup limits not applied: %v\n", vm.ID, err)
	} else if cgroupPath, cgErr := createCgroup(vm.ID, vm.Vcpu, vm.RamMb); cgErr != nil {
		fmt.Printf("[vmmanager] warning: cgroup setup failed for %s: %v\n", vm.ID, cgErr)
	} else if err := addProcessToCgroup(cgroupPath, pid); err != nil {
		fmt.Printf("[vmmanager] warning: failed to add pid %d to cgroup for %s: %v\n", pid, vm.ID, err)
	}

	b.mu.Lock()
	b.machines[vm.ID] = machine
	b.mu.Unlock()

	vm.Pid = pid
	vm.DockerEndpoint = fmt.Sprintf("%s:%d", hostPublicIP(b.cfg), nc.DockerPort)
	return nil
}

func (b *FirecrackerBackend) Stop(ctx context.Context, vm *VM) error {
	b.mu.Lock()
	machine, ok := b.machines[vm.ID]
	b.mu.Unlock()

	if ok {
		if err := machine.StopVMM(); err != nil {
			return fmt.Errorf("stop vmm: %w", err)
		}
		b.mu.Lock()
		delete(b.machines, vm.ID)
		b.mu.Unlock()
		return nil
	}

	// No live handle — most likely this agent restarted since the VM was
	// booted. Fall back to killing the persisted PID directly; the tap
	// device / cgroup / rootfs cleanup happens in Manager.Destroy either way.
	if vm.Pid == 0 {
		return nil // nothing we can do; already gone as far as we know
	}
	if err := syscall.Kill(vm.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("kill pid %d: %w", vm.Pid, err)
	}
	return nil
}

// hostPublicIP is the WireGuard address the returned docker_endpoint uses —
// it's the same address this agent's own API listens on, since that's the
// only interface guaranteed reachable from the scheduler side of the tunnel.
func hostPublicIP(cfg config.Config) string {
	host, _, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		return cfg.ListenAddr
	}
	return host
}
