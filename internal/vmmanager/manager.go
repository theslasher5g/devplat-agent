// Package vmmanager owns the slot/capacity model and VM lifecycle. The
// Backend interface separates "which slot, which network, is there room"
// (pure, unit-testable — see manager_test.go) from "actually boot
// Firecracker" (firecracker.go, requires real hardware and isn't exercised
// by tests here).
package vmmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/theslasher5g/devplat-agent/internal/config"
)

var ErrNoCapacity = errors.New("no free VM slots on this host")
var ErrNotFound = errors.New("vm not found")

type VM struct {
	ID             string        `json:"id"`
	TeamID         string        `json:"team_id"`
	Slot           int           `json:"slot"`
	Vcpu           int64         `json:"vcpu"`   // hard cap from the requesting team's plan
	RamMb          int64         `json:"ram_mb"` // hard cap from the requesting team's plan
	DockerEndpoint string        `json:"docker_endpoint"`
	Pid            int           `json:"pid"`
	CreatedAt      time.Time     `json:"created_at"`
	TTL            time.Duration `json:"ttl"`
}

func (vm *VM) ExpiresAt() time.Time { return vm.CreatedAt.Add(vm.TTL) }

// Backend does the actual privileged work of running a microVM. Boot must
// fill in vm.DockerEndpoint and vm.Pid on success.
type Backend interface {
	Boot(ctx context.Context, vm *VM, nc NetConfig, rootfsPath string) error
	// Stop must be safe to call even if this process never called Boot for
	// this vm (e.g. after an agent restart) — implementations fall back to
	// killing vm.Pid directly. See firecracker.go.
	Stop(ctx context.Context, vm *VM) error
}

type Manager struct {
	cfg     config.Config
	backend Backend

	mu    sync.Mutex
	vms   map[string]*VM // by vm id
	slots map[int]string // slot -> vm id
}

func New(cfg config.Config, backend Backend) (*Manager, error) {
	m := &Manager{
		cfg:     cfg,
		backend: backend,
		vms:     map[string]*VM{},
		slots:   map[int]string{},
	}
	if err := m.reconcile(); err != nil {
		return nil, fmt.Errorf("reconcile existing VM state: %w", err)
	}
	return m, nil
}

// reconcile rebuilds in-memory slot/VM state from per-VM metadata files on
// disk, so an agent restart doesn't "forget" VMs that are still running (or
// still occupying a slot that must not be double-allocated) and doesn't
// leak the TTL tracking the reaper depends on.
func (m *Manager) reconcile() error {
	entries, err := os.ReadDir(m.cfg.VMStateDir)
	if os.IsNotExist(err) {
		return os.MkdirAll(m.cfg.VMStateDir, 0o755)
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		metaPath := filepath.Join(m.cfg.VMStateDir, e.Name(), "meta.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			continue // no metadata — not one of ours, or a half-created dir; leave it
		}
		var vm VM
		if err := json.Unmarshal(data, &vm); err != nil {
			continue
		}
		m.vms[vm.ID] = &vm
		m.slots[vm.Slot] = vm.ID
	}
	return nil
}

func (m *Manager) persist(vm *VM) error {
	dir := filepath.Join(m.cfg.VMStateDir, vm.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(vm)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "meta.json"), data, 0o644)
}

// capacityCPU / capacityRAMMb are the host-wide resource budgets a VM's
// requested size is admitted against. VMs are now variable-sized (per the
// requesting team's plan), so capacity is a raw CPU/RAM budget rather than a
// fixed count of equal slots.
func (m *Manager) capacityCPU() int64 { return detectCPUTotal() }

func (m *Manager) capacityRAMMb() int64 {
	ram := detectRamTotalMb() - m.cfg.HostReservedRamMb
	if ram < 0 {
		ram = 0
	}
	return ram
}

// usedCPULocked / usedRAMMbLocked sum committed resources across running VMs.
// Callers must hold m.mu.
func (m *Manager) usedCPULocked() int64 {
	var sum int64
	for _, vm := range m.vms {
		sum += vm.Vcpu
	}
	return sum
}

func (m *Manager) usedRAMMbLocked() int64 {
	var sum int64
	for _, vm := range m.vms {
		sum += vm.RamMb
	}
	return sum
}

// freeSlot returns the lowest unused slot index (for tap/network derivation).
// The ceiling is capacityCPU(): since every VM needs at least 1 vCPU, the host
// can never run more concurrent VMs than it has CPUs, so that's a safe upper
// bound on distinct slot indices. Callers must hold m.mu.
func (m *Manager) freeSlot() (int, bool) {
	max := int(m.capacityCPU())
	for i := 0; i < max; i++ {
		if _, taken := m.slots[i]; !taken {
			return i, true
		}
	}
	return 0, false
}

func newVMID() string {
	return fmt.Sprintf("vm_%d", time.Now().UnixNano())
}

func (m *Manager) Create(ctx context.Context, teamID string, ttlMinutes int, vcpu, ramMb int64) (*VM, error) {
	if ttlMinutes <= 0 {
		ttlMinutes = m.cfg.DefaultTTLMinutes
	}
	if vcpu <= 0 || ramMb <= 0 {
		return nil, fmt.Errorf("vcpu and ram_mb must be positive (got %d vcpu, %d MB)", vcpu, ramMb)
	}

	m.mu.Lock()
	// Admit against the host-wide resource budget: this VM's requested size
	// plus everything already running must fit within CPU and RAM capacity.
	if m.usedCPULocked()+vcpu > m.capacityCPU() || m.usedRAMMbLocked()+ramMb > m.capacityRAMMb() {
		m.mu.Unlock()
		return nil, ErrNoCapacity
	}
	slot, ok := m.freeSlot()
	if !ok {
		m.mu.Unlock()
		return nil, ErrNoCapacity
	}
	vm := &VM{
		ID:        newVMID(),
		TeamID:    teamID,
		Slot:      slot,
		Vcpu:      vcpu,
		RamMb:     ramMb,
		CreatedAt: time.Now(),
		TTL:       time.Duration(ttlMinutes) * time.Minute,
	}
	// Reserve the slot immediately (before the potentially-slow boot) so a
	// concurrent Create can't also pick it.
	m.slots[slot] = vm.ID
	m.mu.Unlock()

	nc := deriveNetConfig(m.cfg, slot)
	rootfsPath := filepath.Join(m.cfg.VMStateDir, vm.ID, "rootfs.ext4")

	rootfsStart := time.Now()
	if err := prepareRootfs(m.cfg.GoldenImagePath, rootfsPath); err != nil {
		m.releaseSlot(slot)
		return nil, fmt.Errorf("prepare rootfs: %w", err)
	}
	fmt.Printf("[vmmanager] %s: rootfs prepared in %s\n", vm.ID, time.Since(rootfsStart))

	bootStart := time.Now()
	if err := m.backend.Boot(ctx, vm, nc, rootfsPath); err != nil {
		m.releaseSlot(slot)
		_ = removeVMDir(filepath.Join(m.cfg.VMStateDir, vm.ID))
		return nil, fmt.Errorf("boot vm: %w", err)
	}
	fmt.Printf("[vmmanager] %s: Boot() total %s\n", vm.ID, time.Since(bootStart))

	if err := m.persist(vm); err != nil {
		// The VM is running; losing its metadata would leak the slot on
		// restart, but failing the request now would leave an unreachable
		// running VM. Log-and-continue is the lesser evil here.
		fmt.Printf("[vmmanager] warning: failed to persist metadata for %s: %v\n", vm.ID, err)
	}

	m.mu.Lock()
	m.vms[vm.ID] = vm
	m.mu.Unlock()
	return vm, nil
}

func (m *Manager) releaseSlot(slot int) {
	m.mu.Lock()
	delete(m.slots, slot)
	m.mu.Unlock()
}

func (m *Manager) Destroy(ctx context.Context, id string) error {
	fmt.Printf("[vmmanager] Destroy called for %s\n", id)
	m.mu.Lock()
	vm, ok := m.vms[id]
	m.mu.Unlock()
	if !ok {
		return ErrNotFound
	}

	if err := m.backend.Stop(ctx, vm); err != nil {
		return fmt.Errorf("stop vm: %w", err)
	}
	nc := deriveNetConfig(m.cfg, vm.Slot)
	_ = teardownFirewall(m.cfg, nc)
	_ = teardownTapDevice(nc)
	_ = removeCgroup(vm.ID)
	_ = removeVMDir(filepath.Join(m.cfg.VMStateDir, vm.ID))

	m.mu.Lock()
	delete(m.vms, id)
	delete(m.slots, vm.Slot)
	m.mu.Unlock()
	return nil
}

func (m *Manager) List() []*VM {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*VM, 0, len(m.vms))
	for _, vm := range m.vms {
		out = append(out, vm)
	}
	return out
}

func (m *Manager) Get(id string) (*VM, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	vm, ok := m.vms[id]
	return vm, ok
}

// GuestAddr returns "<guest IP>:<port>" for one VM — the address of an
// arbitrary port inside the guest as reachable from THIS host over the VM's
// tap link (the same path waitForDockerReady uses for :2375). The guest IP
// is a pure function of the VM's slot, so this works even for VMs restored
// by reconcile() after an agent restart. Used by the API's per-port proxy
// (api/server.go) to reach container ports Docker published inside the
// guest — those are only DNAT'd guest-side, so unlike the Docker API port
// there is no host-side DNAT for them.
func (m *Manager) GuestAddr(id string, port int) (string, error) {
	m.mu.Lock()
	vm, ok := m.vms[id]
	m.mu.Unlock()
	if !ok {
		return "", ErrNotFound
	}
	nc := deriveNetConfig(m.cfg, vm.Slot)
	return net.JoinHostPort(nc.GuestIP.String(), strconv.Itoa(port)), nil
}

type HealthStatus struct {
	CPUTotal      int64
	CPUUsed       int64
	RAMTotalMb    int64
	RAMUsedMb     int64
	ActiveVMCount int
}

func (m *Manager) Health() HealthStatus {
	m.mu.Lock()
	active := len(m.vms)
	usedCPU := m.usedCPULocked()
	usedRAM := m.usedRAMMbLocked()
	m.mu.Unlock()
	return HealthStatus{
		CPUTotal:      m.capacityCPU(),
		CPUUsed:       usedCPU,
		RAMTotalMb:    m.capacityRAMMb(),
		RAMUsedMb:     usedRAM,
		ActiveVMCount: active,
	}
}

// ReapExpired destroys every VM past its TTL and returns their ids —
// independent of whatever cleanup Testcontainers' own Ryuk does client-side,
// this is the server-side backstop the work order requires.
func (m *Manager) ReapExpired(ctx context.Context) []string {
	now := time.Now()
	m.mu.Lock()
	var expired []string
	for id, vm := range m.vms {
		if now.After(vm.ExpiresAt()) {
			expired = append(expired, id)
		}
	}
	m.mu.Unlock()

	var reaped []string
	for _, id := range expired {
		if err := m.Destroy(ctx, id); err != nil {
			fmt.Printf("[reaper] failed to destroy expired vm %s: %v\n", id, err)
			continue
		}
		reaped = append(reaped, id)
	}
	return reaped
}
