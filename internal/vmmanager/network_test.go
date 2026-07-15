package vmmanager

import (
	"net"
	"strconv"
	"testing"

	"github.com/theslasher5g/devplat-agent/internal/config"
)

func TestDeriveNetConfig_DistinctNonOverlappingSlots(t *testing.T) {
	cfg := config.Config{
		TapIPBase:      net.ParseIP("172.20.0.0").To4(),
		DockerPortBase: 10000,
	}

	seen := map[string]int{} // ip -> slot that claimed it, to catch collisions
	for slot := 0; slot < 200; slot++ {
		nc := deriveNetConfig(cfg, slot)

		if nc.TapName != "fc-tap-"+strconv.Itoa(slot) {
			t.Fatalf("slot %d: unexpected tap name %q", slot, nc.TapName)
		}
		if nc.DockerPort != 10000+slot {
			t.Fatalf("slot %d: expected docker port %d, got %d", slot, 10000+slot, nc.DockerPort)
		}
		// host and guest IP must be adjacent within the /30 and distinct
		// from every other slot's addresses.
		for _, ip := range []net.IP{nc.HostIP, nc.GuestIP} {
			key := ip.String()
			if prev, dup := seen[key]; dup {
				t.Fatalf("IP %s reused by slot %d and slot %d", key, prev, slot)
			}
			seen[key] = slot
		}
		ones, _ := nc.Mask.Size()
		if ones != 30 {
			t.Fatalf("slot %d: expected a /30 mask, got /%d", slot, ones)
		}
	}
}

func TestDeriveNetConfig_Deterministic(t *testing.T) {
	cfg := config.Config{TapIPBase: net.ParseIP("172.20.0.0").To4(), DockerPortBase: 10000}
	a := deriveNetConfig(cfg, 42)
	b := deriveNetConfig(cfg, 42)
	if a.HostIP.String() != b.HostIP.String() || a.GuestIP.String() != b.GuestIP.String() || a.DockerPort != b.DockerPort {
		t.Fatalf("deriveNetConfig must be a pure function of (cfg, slot): got %+v and %+v", a, b)
	}
}
