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
	"os"
	"path/filepath"
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

func (m *Manager) capacitySlots() int {
	cpuTotal := detectCPUTotal()
	ramTotal := detectRamTotalMb() - m.cfg.HostReservedRamMb
	if ramTotal < 0 {
		ramTotal = 0
	}
	byCPU := int(cpuTotal / m.cfg.VMVcpus)
	byRAM := int(ramTotal / m.cfg.VMRamMb)
	if byCPU < byRAM {
		return byCPU
	}
	return byRAM
}

func (m *Manager) freeSlot() (int, bool) {
	max := m.capacitySlots()
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

func (m *Manager) Create(ctx context.Context, teamID string, ttlMinutes int) (*VM, error) {
	if ttlMinutes <= 0 {
		ttlMinutes = m.cfg.DefaultTTLMinutes
	}

	m.mu.Lock()
	slot, ok := m.freeSlot()
	if !ok {
		m.mu.Unlock()
		return nil, ErrNoCapacity
	}
	vm := &VM{
		ID:        newVMID(),
		TeamID:    teamID,
		Slot:      slot,
		CreatedAt: time.Now(),
		TTL:       time.Duration(ttlMinutes) * time.Minute,
	}
	// Reserve the slot immediately (before the potentially-slow boot) so a
	// concurrent Create can't also pick it.
	m.slots[slot] = vm.ID
	m.mu.Unlock()

	nc := deriveNetConfig(m.cfg, slot)
	rootfsPath := filepath.Join(m.cfg.VMStateDir, vm.ID, "rootfs.ext4")

	if err := prepareRootfs(m.cfg.GoldenImagePath, rootfsPath); err != nil {
		m.releaseSlot(slot)
		return nil, fmt.Errorf("prepare rootfs: %w", err)
	}

	if err := m.backend.Boot(ctx, vm, nc, rootfsPath); err != nil {
		m.releaseSlot(slot)
		_ = removeVMDir(filepath.Join(m.cfg.VMStateDir, vm.ID))
		return nil, fmt.Errorf("boot vm: %w", err)
	}

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
	m.mu.Unlock()
	return HealthStatus{
		CPUTotal:      int64(m.capacitySlots()) * m.cfg.VMVcpus,
		CPUUsed:       int64(active) * m.cfg.VMVcpus,
		RAMTotalMb:    int64(m.capacitySlots()) * m.cfg.VMRamMb,
		RAMUsedMb:     int64(active) * m.cfg.VMRamMb,
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
