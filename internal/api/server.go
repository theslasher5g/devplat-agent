// Package api is the agent's HTTP surface, reachable only on the host's
// WireGuard address (see config.ListenAddr) — this must never be bound to
// 0.0.0.0 or a public interface.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/theslasher5g/devplat-agent/internal/vmmanager"
)

type Server struct {
	manager *vmmanager.Manager
	token   string
	mux     *http.ServeMux
	// draining flips to true on SIGTERM (see cmd/agent/main.go) — new VM
	// creation is refused and it's reported on GET /health so the scheduler
	// stops assigning here without existing VMs being force-killed.
	draining atomic.Bool
}

func NewServer(manager *vmmanager.Manager, token string) *Server {
	s := &Server{manager: manager, token: token, mux: http.NewServeMux()}
	s.mux.HandleFunc("POST /vms", s.handleCreateVM)
	s.mux.HandleFunc("DELETE /vms/{id}", s.handleDeleteVM)
	s.mux.HandleFunc("GET /vms", s.handleListVMs)
	s.mux.HandleFunc("GET /vms/{id}/proxy/{port}", s.handleProxyPort)
	s.mux.HandleFunc("GET /health", s.handleHealth)
	return s
}

func (s *Server) SetDraining(v bool) { s.draining.Store(v) }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	// Constant-time compare so a byte-by-byte timing difference can't be used
	// to recover the token. The listener is WireGuard-only, but the per-port
	// proxy makes a leaked token especially valuable (raw TCP into any VM on
	// the host), so don't rely solely on the network boundary here.
	expected := "Bearer " + s.token
	if subtle.ConstantTimeCompare([]byte(auth), []byte(expected)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_agent_token"})
		return
	}
	s.mux.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[api] failed to encode response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

type createVMRequest struct {
	TeamID     string `json:"team_id"`
	TTLMinutes int    `json:"ttl_minutes"`
	// Hard per-VM resource caps, set by the scheduler from the requesting
	// team's plan. Required — the agent refuses a VM with no explicit size
	// rather than silently falling back to a default it can't account for.
	Vcpu  int64 `json:"vcpu"`
	RamMb int64 `json:"ram_mb"`
}

func (s *Server) handleCreateVM(w http.ResponseWriter, r *http.Request) {
	if s.draining.Load() {
		writeError(w, http.StatusServiceUnavailable, "host_draining")
		return
	}
	var req createVMRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body")
		return
	}
	if req.TeamID == "" {
		writeError(w, http.StatusBadRequest, "team_id_required")
		return
	}
	if req.Vcpu <= 0 || req.RamMb <= 0 {
		writeError(w, http.StatusBadRequest, "vcpu_and_ram_mb_required")
		return
	}

	// Needs real headroom over the boot-readiness wait inside Boot()
	// (waitForDockerReady alone budgets up to 30s) plus tap/firewall setup,
	// the rootfs copy, and the Firecracker start itself — VMs were observed
	// dying ~18s into the readiness wait instead of at its own 30s deadline,
	// which only makes sense if this outer context's deadline (previously
	// 45s) was being hit first because the earlier setup steps ate more of
	// the budget than expected. Generous margin here costs nothing on the
	// happy path; it just avoids this handler's own timeout preempting the
	// readiness wait's own, more informative, timeout/diagnostic dump.
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	vm, err := s.manager.Create(ctx, req.TeamID, req.TTLMinutes, req.Vcpu, req.RamMb)
	if err != nil {
		if errors.Is(err, vmmanager.ErrNoCapacity) {
			writeError(w, http.StatusConflict, "no_capacity")
			return
		}
		log.Printf("[api] create vm failed: %v", err)
		writeError(w, http.StatusInternalServerError, "create_failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"vm_id":           vm.ID,
		"docker_endpoint": vm.DockerEndpoint,
	})
}

func (s *Server) handleDeleteVM(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if err := s.manager.Destroy(ctx, id); err != nil {
		if errors.Is(err, vmmanager.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found")
			return
		}
		log.Printf("[api] destroy vm %s failed: %v", id, err)
		writeError(w, http.StatusInternalServerError, "destroy_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleListVMs(w http.ResponseWriter, _ *http.Request) {
	vms := s.manager.List()
	out := make([]map[string]any, 0, len(vms))
	for _, vm := range vms {
		out = append(out, map[string]any{
			"vm_id":           vm.ID,
			"team_id":         vm.TeamID,
			"docker_endpoint": vm.DockerEndpoint,
			"created_at":      vm.CreatedAt,
			"expires_at":      vm.ExpiresAt(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"vms": out})
}

// handleProxyPort upgrades this HTTP connection into a raw bidirectional
// TCP pipe to an arbitrary port on one VM's guest IP, dialed over the tap
// link this host owns. It exists for Testcontainers port mapping: ports
// Docker publishes inside the guest are only DNAT'd guest-side (dockerd's
// own NAT chains), so unlike the fixed Docker API port there is no
// host-side DNAT a remote caller could hit — the backend's per-port tunnel
// (devplat-backend/src/routes/tunnel.ts) calls this instead, one upgraded
// connection per client TCP connection.
//
// Security model: same Bearer token as every other endpoint (enforced in
// ServeHTTP before routing), reachable only on the WireGuard-bound listen
// address, and the dial target is derived strictly from the VM's own slot —
// a caller can pick the port but never the IP, so one team's tunnel can't
// be steered at another VM (the backend enforces which team may name this
// VM at all, exactly like the existing docker_endpoint tunnel).
//
// Deliberately NOT draining-gated: draining refuses new VMs but leaves
// existing ones running, and their tunnels must keep working.
func (s *Server) handleProxyPort(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		writeError(w, http.StatusBadRequest, "invalid_port")
		return
	}
	addr, err := s.manager.GuestAddr(id, port)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	// Dial the guest BEFORE hijacking, so a dead/refusing port is still a
	// clean HTTP error the backend can log and surface, not a mid-stream cut.
	guest, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		log.Printf("[api] proxy dial %s for vm %s failed: %v", addr, id, err)
		writeError(w, http.StatusBadGateway, "guest_dial_failed")
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = guest.Close()
		writeError(w, http.StatusInternalServerError, "hijack_unsupported")
		return
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		_ = guest.Close()
		log.Printf("[api] proxy hijack for vm %s failed: %v", id, err)
		return
	}
	defer conn.Close()
	defer guest.Close()
	// The server may have armed read/write deadlines on this conn before we
	// took it over; a long-lived pipe (a test run holding a DB connection
	// open for minutes) must not be killed by them.
	_ = conn.SetDeadline(time.Time{})

	if _, err := bufrw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: tcp\r\nConnection: Upgrade\r\n\r\n"); err != nil {
		return
	}
	if err := bufrw.Flush(); err != nil {
		return
	}

	// bufrw.Reader may already hold bytes the client sent right after its
	// upgrade request — reading via bufrw (not conn directly) forwards them
	// instead of dropping them. First side to finish tears down both (the
	// deferred Closes), matching how the backend relay handles termination.
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(guest, bufrw)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, guest)
		done <- struct{}{}
	}()
	<-done
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	h := s.manager.Health()
	writeJSON(w, http.StatusOK, map[string]any{
		"cpu_total":       h.CPUTotal,
		"cpu_used":        h.CPUUsed,
		"ram_total_mb":    h.RAMTotalMb,
		"ram_used_mb":     h.RAMUsedMb,
		"active_vm_count": h.ActiveVMCount,
		"draining":        s.draining.Load(),
	})
}
