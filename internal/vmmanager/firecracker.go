package vmmanager

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
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
	consolePath := filepath.Join(b.cfg.VMStateDir, vm.ID, "console.log")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return fmt.Errorf("create vm state dir: %w", err)
	}
	// LogPath (below) is Firecracker's own VMM-level log, not the guest's
	// serial console — despite what the KernelArgs comment used to imply,
	// ttyS0 output was never actually captured anywhere: VMCommandBuilder
	// doesn't wire stdout/stderr unless told to, so it was silently
	// discarded.
	//
	// A plain os.File as Firecracker's stdout/stderr looked like it should
	// work but didn't: the guest never produced a single byte of userspace
	// output even though kernel messages came through fine, and a manual
	// side-by-side test confirmed why — running Firecracker attached to a
	// real terminal, the exact same kernel+rootfs booted and produced
	// init.sh's output immediately. Something about the serial console
	// path only works when Firecracker's stdout is an actual tty. A pty
	// gives it one: the slave end goes to Firecracker as a real terminal,
	// and a goroutine copies everything from the master end into the log
	// file for after-the-fact debugging.
	ptmx, ttyDev, err := pty.Open()
	if err != nil {
		return fmt.Errorf("open pty for console: %w", err)
	}
	consoleFile, err := os.OpenFile(consolePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		_ = ttyDev.Close()
		_ = ptmx.Close()
		return fmt.Errorf("open console log: %w", err)
	}
	go func() {
		defer consoleFile.Close()
		_, _ = io.Copy(consoleFile, ptmx)
	}()

	fcCfg := firecracker.Config{
		SocketPath:      socketPath,
		LogPath:         logPath,
		LogLevel:        "Warning",
		KernelImagePath: b.cfg.KernelImagePath,
		// console on ttyS0 — Firecracker writes that to its own process
		// stdout/stderr (wired to the pty below), for boot-failure
		// debugging; reboot=k makes a guest kernel panic kill the VM
		// instead of looping, so the reaper doesn't have to distinguish
		// "slow" from "stuck".
		//
		// random.trust_cpu=on asks the kernel to seed its CRNG from the CPU's
		// hardware RNG (RDRAND) so a headless guest with no entropy sources
		// doesn't stall for ~80s initializing randomness — which blocked
		// dockerd's getrandom() call and made every boot look like a hang.
		// NOTE: this option only exists on Linux 4.19+; this guest's kernel
		// is 4.14 and ignores it, so the ACTUAL fix is haveged running early
		// in the guest's init.sh. This arg is kept only as belt-and-braces
		// for the day the guest kernel is bumped past 4.19.
		KernelArgs: "console=ttyS0 reboot=k panic=1 pci=off random.trust_cpu=on",
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
		WithStdout(ttyDev).
		WithStderr(ttyDev).
		Build(ctx)
	// Handing the child an fd that happens to be a pty isn't enough on its
	// own — without this, Firecracker never gets the pty as its controlling
	// terminal (no session leader, no TIOCSCTTY), so the kernel's tty layer
	// doesn't treat ttyS0 the way it does for a real login shell. Ctty:1
	// indexes cmd's Stdout slot (the pty slave), matching WithStdout above.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 1}

	machine, err := firecracker.NewMachine(ctx, fcCfg, firecracker.WithProcessRunner(cmd))
	if err != nil {
		_ = ttyDev.Close()
		_ = ptmx.Close()
		_ = teardownFirewall(b.cfg, nc)
		_ = teardownTapDevice(nc)
		return fmt.Errorf("configure machine: %w", err)
	}
	if err := machine.Start(ctx); err != nil {
		_ = ttyDev.Close()
		_ = ptmx.Close()
		_ = teardownFirewall(b.cfg, nc)
		_ = teardownTapDevice(nc)
		return fmt.Errorf("start machine: %w", err)
	}
	// From here the subprocess owns its own duplicated fd for the tty slave
	// — safe to close our handle without affecting what it writes. Keep
	// ptmx (the master) open: the copy goroutine reads from it until
	// Firecracker exits and closes its end, at which point io.Copy hits
	// EOF and the goroutine closes consoleFile itself.
	_ = ttyDev.Close()

	pid, err := machine.PID()
	if err != nil {
		fmt.Printf("[vmmanager] warning: could not get PID for %s, cgroup limits not applied: %v\n", vm.ID, err)
	} else if cgroupPath, cgErr := createCgroup(vm.ID, vm.Vcpu, vm.RamMb); cgErr != nil {
		fmt.Printf("[vmmanager] warning: cgroup setup failed for %s: %v\n", vm.ID, cgErr)
	} else if err := addProcessToCgroup(cgroupPath, pid); err != nil {
		fmt.Printf("[vmmanager] warning: failed to add pid %d to cgroup for %s: %v\n", pid, vm.ID, err)
	}

	// Start() only means the Firecracker process launched and the guest
	// kernel began booting — not that init.sh has run, containerd is up,
	// and dockerd is actually listening. Returning success (and a
	// docker_endpoint) before that point hands the scheduler an endpoint
	// that isn't ready yet: the client's first request lands mid-boot and
	// gets nothing back (an abrupt EOF, not a clean refusal), even though
	// the VM comes up fine a couple seconds later. Dial the guest directly
	// on the tap network (no DNAT/WireGuard hop needed, this host owns
	// that link) until dockerd answers, so "assigned" actually means ready.
	// dockerd's own ~15s intentional stall for the insecure-TCP-without-TLS
	// deprecation warning, plus containerd startup, plus real boot variance
	// on constrained single-vCPU hardware — 30s leaves comfortable margin
	// now that the entropy-starvation issue (60s+ blocked in crypto/rand)
	// is fixed via random.trust_cpu=on.
	if err := waitForDockerReady(ctx, nc.GuestIP, 30*time.Second); err != nil {
		_ = machine.StopVMM()
		_ = ptmx.Close() // stopping the VMM closes the slave; we still own the master
		_ = teardownFirewall(b.cfg, nc)
		_ = teardownTapDevice(nc)
		return fmt.Errorf("boot vm: %w", err)
	}

	b.mu.Lock()
	b.machines[vm.ID] = machine
	b.mu.Unlock()

	vm.Pid = pid
	vm.DockerEndpoint = fmt.Sprintf("%s:%d", hostPublicIP(b.cfg), nc.DockerPort)
	return nil
}

// waitForDockerReady polls the guest's Docker port directly over the tap
// link until it accepts a TCP connection or timeout elapses.
func waitForDockerReady(ctx context.Context, guestIP net.IP, timeout time.Duration) error {
	addr := net.JoinHostPort(guestIP.String(), "2375")
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("guest docker daemon at %s not ready after %s: %w", addr, timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
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
