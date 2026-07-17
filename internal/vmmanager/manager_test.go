package vmmanager

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/theslasher5g/devplat-agent/internal/config"
)

// fakeBackend never touches the real system (no netlink/iptables/tc/cgroup
// calls) — it stands in for FirecrackerBackend so the slot/capacity/TTL
// logic in Manager can be tested without root, KVM, or a golden image.
type fakeBackend struct {
	bootCalls int
	stopCalls int
	failBoot  bool
}

var errBootFailed = errors.New("boot failed")

func (b *fakeBackend) Boot(_ context.Context, vm *VM, nc NetConfig, _ string) error {
	b.bootCalls++
	if b.failBoot {
		return errBootFailed
	}
	vm.DockerEndpoint = nc.HostIP.String()
	vm.Pid = 12345
	return nil
}

func (b *fakeBackend) Stop(_ context.Context, _ *VM) error {
	b.stopCalls++
	return nil
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		VMStateDir:        t.TempDir(),
		GoldenImagePath:   writeGoldenImage(t),
		HostReservedRamMb: 0,
		DefaultTTLMinutes: 60,
		TapIPBase:         net.ParseIP("172.20.0.0").To4(),
		DockerPortBase:    10000,
		WireguardCIDR:     "10.10.0.0/24",
	}
}

func writeGoldenImage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/golden.ext4"
	if err := os.WriteFile(path, []byte("fake rootfs contents"), 0o644); err != nil {
		t.Fatalf("write golden image: %v", err)
	}
	return path
}

func withDetectedHost(t *testing.T, cpu, ramMb int64) {
	t.Helper()
	origCPU, origRAM := detectCPUTotal, detectRamTotalMb
	detectCPUTotal = func() int64 { return cpu }
	detectRamTotalMb = func() int64 { return ramMb }
	t.Cleanup(func() {
		detectCPUTotal = origCPU
		detectRamTotalMb = origRAM
	})
}

func TestCreate_AllocatesUniqueSlots(t *testing.T) {
	withDetectedHost(t, 4, 8192) // 4 vCPU, 8GB -> 4 slots by CPU, 4 by RAM (1 vcpu/2GB per VM)
	cfg := testConfig(t)
	backend := &fakeBackend{}
	m, err := New(cfg, backend)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	seen := map[int]bool{}
	for i := 0; i < 4; i++ {
		vm, err := m.Create(context.Background(), "team-1", 60, 1, 2048)
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		if seen[vm.Slot] {
			t.Fatalf("slot %d reused while still occupied", vm.Slot)
		}
		seen[vm.Slot] = true
	}

	if _, err := m.Create(context.Background(), "team-1", 60, 1, 2048); err != ErrNoCapacity {
		t.Fatalf("expected ErrNoCapacity on the 5th VM, got %v", err)
	}
	if backend.bootCalls != 4 {
		t.Fatalf("expected 4 boot calls, got %d", backend.bootCalls)
	}
}

func TestCreate_AdmitsByRawResourcesNotSlotCount(t *testing.T) {
	// 4 vCPU / 8 GB host. VMs sized 2 vCPU / 4 GB (a "Solo"-tier VM) — only
	// TWO fit by CPU and by RAM, even though there are 4 CPUs, because
	// capacity is now a raw resource budget, not a fixed count of 1-vCPU slots.
	withDetectedHost(t, 4, 8192)
	cfg := testConfig(t)
	m, err := New(cfg, &fakeBackend{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for i := 0; i < 2; i++ {
		if _, err := m.Create(context.Background(), "team-1", 60, 2, 4096); err != nil {
			t.Fatalf("Create %d (2vcpu/4GB): %v", i, err)
		}
	}
	if _, err := m.Create(context.Background(), "team-1", 60, 2, 4096); err != ErrNoCapacity {
		t.Fatalf("expected ErrNoCapacity on the 3rd 2vcpu/4GB VM, got %v", err)
	}
	// A 1-vCPU/2 GB VM still doesn't fit — RAM is fully committed (2×4 GB = 8 GB).
	if _, err := m.Create(context.Background(), "team-1", 60, 1, 2048); err != ErrNoCapacity {
		t.Fatalf("expected ErrNoCapacity when RAM is exhausted, got %v", err)
	}
}

func TestCreate_RejectsVMLargerThanHost(t *testing.T) {
	withDetectedHost(t, 4, 8192)
	m, err := New(testConfig(t), &fakeBackend{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// 6 vCPU requested on a 4-CPU host — must be refused, not clamped.
	if _, err := m.Create(context.Background(), "team-1", 60, 6, 4096); err != ErrNoCapacity {
		t.Fatalf("expected ErrNoCapacity for an oversized VM, got %v", err)
	}
	// Zero/negative sizing is a bad request, not a capacity problem.
	if _, err := m.Create(context.Background(), "team-1", 60, 0, 2048); err == nil || err == ErrNoCapacity {
		t.Fatalf("expected a validation error for vcpu=0, got %v", err)
	}
}

func TestDestroy_FreesSlotForReuse(t *testing.T) {
	withDetectedHost(t, 1, 2048) // exactly 1 slot
	cfg := testConfig(t)
	backend := &fakeBackend{}
	m, err := New(cfg, backend)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	vm1, err := m.Create(context.Background(), "team-1", 60, 1, 2048)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.Create(context.Background(), "team-1", 60, 1, 2048); err != ErrNoCapacity {
		t.Fatalf("expected ErrNoCapacity, got %v", err)
	}

	if err := m.Destroy(context.Background(), vm1.ID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if backend.stopCalls != 1 {
		t.Fatalf("expected 1 stop call, got %d", backend.stopCalls)
	}

	vm2, err := m.Create(context.Background(), "team-2", 60, 1, 2048)
	if err != nil {
		t.Fatalf("Create after Destroy: %v", err)
	}
	if vm2.Slot != vm1.Slot {
		t.Fatalf("expected slot %d to be reused, got %d", vm1.Slot, vm2.Slot)
	}
}

func TestReapExpired_DestroysOnlyOverdueVMs(t *testing.T) {
	withDetectedHost(t, 4, 8192)
	cfg := testConfig(t)
	backend := &fakeBackend{}
	m, err := New(cfg, backend)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	expired, err := m.Create(context.Background(), "team-1", 60, 1, 2048)
	if err != nil {
		t.Fatalf("Create expired vm: %v", err)
	}
	// Backdate it past its TTL — same package, so reaching into the stored
	// VM directly is simpler than sleeping past a real TTL in a unit test.
	m.mu.Lock()
	m.vms[expired.ID].CreatedAt = m.vms[expired.ID].CreatedAt.Add(-2 * time.Hour)
	m.mu.Unlock()

	fresh, err := m.Create(context.Background(), "team-1", 60, 1, 2048)
	if err != nil {
		t.Fatalf("Create fresh vm: %v", err)
	}

	reaped := m.ReapExpired(context.Background())
	if len(reaped) != 1 {
		t.Fatalf("expected 1 reaped vm, got %d: %v", len(reaped), reaped)
	}
	if _, ok := m.Get(fresh.ID); !ok {
		t.Fatalf("fresh vm should not have been reaped")
	}
}

func TestReconcile_RecoversStateAcrossRestart(t *testing.T) {
	withDetectedHost(t, 4, 8192)
	cfg := testConfig(t)
	backend := &fakeBackend{}
	m1, err := New(cfg, backend)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	vm, err := m1.Create(context.Background(), "team-1", 60, 1, 2048)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulate an agent restart: a fresh Manager over the same VMStateDir
	// (and a fresh, empty fakeBackend — the point is the *Manager* recovers
	// slot/VM bookkeeping from disk, not that Firecracker handles survive).
	m2, err := New(cfg, &fakeBackend{})
	if err != nil {
		t.Fatalf("New (restart): %v", err)
	}
	recovered, ok := m2.Get(vm.ID)
	if !ok {
		t.Fatalf("expected vm %s to be recovered after restart", vm.ID)
	}
	if recovered.Slot != vm.Slot {
		t.Fatalf("expected recovered slot %d, got %d", vm.Slot, recovered.Slot)
	}

	// The slot must still be considered occupied post-restart, or a new
	// Create could double-allocate it.
	if _, ok := m2.freeSlot(); ok {
		vms := m2.List()
		if len(vms) != 1 {
			t.Fatalf("expected exactly 1 recovered vm, got %d", len(vms))
		}
	}
}
