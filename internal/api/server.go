// Package api is the agent's HTTP surface, reachable only on the host's
// WireGuard address (see config.ListenAddr) — this must never be bound to
// 0.0.0.0 or a public interface.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
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
	s.mux.HandleFunc("GET /health", s.handleHealth)
	return s
}

func (s *Server) SetDraining(v bool) { s.draining.Store(v) }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if auth != "Bearer "+s.token {
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
	// (waitForDockerReady alone budgets up to 30s) plus tap/firewall setup
	// and the Firecracker start itself.
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
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
