// Package reaper enforces the hard, server-side VM TTL — independent of
// whatever cleanup Testcontainers' client-side Ryuk does, per the work
// order: a client that crashes or never runs Ryuk must not leak a VM forever.
package reaper

import (
	"context"
	"log"
	"time"

	"github.com/theslasher5g/devplat-agent/internal/vmmanager"
)

func Start(ctx context.Context, manager *vmmanager.Manager, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reaped := manager.ReapExpired(ctx)
			if len(reaped) > 0 {
				log.Printf("[reaper] destroyed %d overdue vm(s): %v", len(reaped), reaped)
			}
		}
	}
}
