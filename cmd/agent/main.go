// Command devplat-agent runs on each data-plane host. It exposes a small
// HTTP API (bound only to the host's WireGuard address) that the scheduler
// uses to create/destroy Firecracker microVMs, enforces a hard per-VM TTL
// independent of client-side cleanup, and reports host status to the
// scheduler. See README.md for the full picture and deploy/ for the
// systemd unit.
package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/theslasher5g/devplat-agent/internal/api"
	"github.com/theslasher5g/devplat-agent/internal/config"
	"github.com/theslasher5g/devplat-agent/internal/heartbeat"
	"github.com/theslasher5g/devplat-agent/internal/reaper"
	"github.com/theslasher5g/devplat-agent/internal/vmmanager"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	backend := vmmanager.NewFirecrackerBackend(cfg)
	manager, err := vmmanager.New(cfg, backend)
	if err != nil {
		log.Fatalf("vmmanager: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	server := api.NewServer(manager, cfg.AgentToken, cfg.RegistryMetricsURL)
	httpServer := &http.Server{Addr: cfg.ListenAddr, Handler: server}

	var draining atomic.Bool
	go reaper.Start(ctx, manager, time.Duration(cfg.ReaperIntervalSeconds)*time.Second)
	go heartbeat.Start(ctx, manager, cfg.SchedulerURL, cfg.AgentToken, cfg.RegistryMetricsURL, cfg.HeartbeatInterval, &draining)

	go func() {
		log.Printf("devplat-agent listening on %s (WireGuard-only — do not expose publicly)", cfg.ListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down: marking host draining, existing VMs are left running")
	draining.Store(true)
	server.SetDraining(true)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("http server shutdown: %v", err)
	}
}
