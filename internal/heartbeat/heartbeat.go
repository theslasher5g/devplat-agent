// Package heartbeat pushes this host's status to the scheduler over the
// public internet (not the WireGuard tunnel) — see devplat-backend's
// routes/hosts.ts for why: Postgres has no route from this hardware, so an
// HTTP call to the backend's public API is the only way host status reaches
// the database. This is one half of the health/status picture; the
// scheduler also independently polls GET /health over the tunnel (see the
// backend's scheduler/healthPoller.ts) — the two are redundant on purpose.
package heartbeat

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/theslasher5g/devplat-agent/internal/regmetrics"
	"github.com/theslasher5g/devplat-agent/internal/vmmanager"
)

type payload struct {
	CPUUsed       int64 `json:"cpuUsed"`
	RAMUsedMb     int64 `json:"ramUsedMb"`
	ActiveVMCount int   `json:"activeVmCount"`
	Draining      bool  `json:"draining"`
	// Cumulative registry-cache counters, omitted when the cache's debug
	// endpoint is unreachable (nil), so the scheduler distinguishes "no data"
	// from a real zero.
	CacheLookups *uint64 `json:"cacheLookups,omitempty"`
	CacheHits    *uint64 `json:"cacheHits,omitempty"`
}

func Start(ctx context.Context, manager *vmmanager.Manager, schedulerURL, token, registryMetricsURL string, interval time.Duration, draining *atomic.Bool) {
	client := &http.Client{Timeout: 10 * time.Second}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	send := func() {
		h := manager.Health()
		p := payload{
			CPUUsed:       h.CPUUsed,
			RAMUsedMb:     h.RAMUsedMb,
			ActiveVMCount: h.ActiveVMCount,
			Draining:      draining.Load(),
		}
		if total, hits, ok := regmetrics.Scrape(ctx, registryMetricsURL); ok {
			p.CacheLookups, p.CacheHits = &total, &hits
		}
		body, err := json.Marshal(p)
		if err != nil {
			log.Printf("[heartbeat] marshal failed: %v", err)
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, schedulerURL+"/internal/hosts/heartbeat", bytes.NewReader(body))
		if err != nil {
			log.Printf("[heartbeat] request build failed: %v", err)
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[heartbeat] send failed: %v", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			log.Printf("[heartbeat] scheduler returned %s", resp.Status)
		}
	}

	send() // don't wait a full interval before the first report
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			send()
		}
	}
}
