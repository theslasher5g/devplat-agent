// Package config reads devplat-agent's configuration from the environment.
// See .env.example for the full list with explanations.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

type Config struct {
	// ListenAddr is the WireGuard address:port this agent's HTTP API binds
	// to — e.g. "10.10.0.2:7777". Never bind 0.0.0.0: the agent must only be
	// reachable through the tunnel from the scheduler, never the public
	// internet.
	ListenAddr string
	// AgentToken is the shared secret issued by POST /admin/hosts on the
	// backend. Used bidirectionally: required as Bearer auth on every
	// incoming request here, and sent as Bearer auth on outgoing heartbeats.
	AgentToken string
	// SchedulerURL is the backend's public base URL (e.g. https://api.devplat.ch).
	// Heartbeats go over the public internet, not the tunnel — see
	// devplat-backend's routes/hosts.ts for why (Postgres has no route from
	// this host, so the tunnel direction that matters is scheduler → agent).
	SchedulerURL      string
	HeartbeatInterval time.Duration

	GoldenImagePath   string // versioned rootfs.ext4, read-only source copied per VM
	KernelImagePath   string // uncompressed vmlinux
	FirecrackerBinary string
	VMStateDir        string // per-VM sockets/rootfs/logs live under here

	// Per-VM sizing (vCPU/RAM) is NOT configured here — it arrives on each
	// POST /vms request as the requesting team's plan cap, set by the
	// scheduler (see devplat-backend's plans table / allocator). This agent
	// only enforces the host-wide resource budget below.
	//
	// HostReservedRamMb is subtracted from detected physical RAM before
	// computing VM capacity — headroom for the host OS, the registry cache
	// container, and this agent itself.
	HostReservedRamMb     int64
	DefaultTTLMinutes     int
	ReaperIntervalSeconds int

	// TapIPBase is the first address of this host's /16 VM network pool
	// (e.g. 172.20.0.0). Each VM gets a /30 out of it derived from its slot
	// index — see vmmanager/network.go.
	TapIPBase net.IP
	// DockerPortBase + slot = the host port DNAT'd to that VM's Docker
	// daemon (2375 inside the guest). Only reachable from WireguardCIDR.
	DockerPortBase    int
	WireguardCIDR     string // e.g. 10.10.0.0/24 — the only source allowed to reach VM docker ports
	BandwidthCapMbit  int
	RegistryMirrorURL string // e.g. http://127.0.0.1:5000, baked into each VM's docker daemon.json
}

func requiredString(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("missing required env var %s", name)
	}
	return v, nil
}

func intEnv(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", name, err)
	}
	return n, nil
}

func Load() (Config, error) {
	var cfg Config
	var err error

	if cfg.ListenAddr, err = requiredString("LISTEN_ADDR"); err != nil {
		return cfg, err
	}
	if cfg.AgentToken, err = requiredString("AGENT_TOKEN"); err != nil {
		return cfg, err
	}
	if cfg.SchedulerURL, err = requiredString("SCHEDULER_URL"); err != nil {
		return cfg, err
	}
	if cfg.GoldenImagePath, err = requiredString("GOLDEN_IMAGE_PATH"); err != nil {
		return cfg, err
	}
	if cfg.KernelImagePath, err = requiredString("KERNEL_IMAGE_PATH"); err != nil {
		return cfg, err
	}
	tapIPBase, err := requiredString("TAP_IP_BASE")
	if err != nil {
		return cfg, err
	}
	cfg.TapIPBase = net.ParseIP(tapIPBase).To4()
	if cfg.TapIPBase == nil {
		return cfg, fmt.Errorf("TAP_IP_BASE %q is not a valid IPv4 address", tapIPBase)
	}
	if cfg.WireguardCIDR, err = requiredString("WIREGUARD_CIDR"); err != nil {
		return cfg, err
	}

	cfg.FirecrackerBinary = envOr("FIRECRACKER_BINARY", "/usr/local/bin/firecracker")
	cfg.VMStateDir = envOr("VM_STATE_DIR", "/var/lib/devplat/vms")
	cfg.RegistryMirrorURL = envOr("REGISTRY_MIRROR_URL", "")

	heartbeatSeconds, err := intEnv("HEARTBEAT_INTERVAL_SECONDS", 20)
	if err != nil {
		return cfg, err
	}
	cfg.HeartbeatInterval = time.Duration(heartbeatSeconds) * time.Second

	hostReserved, err := intEnv("HOST_RESERVED_RAM_MB", 8192)
	if err != nil {
		return cfg, err
	}
	cfg.HostReservedRamMb = int64(hostReserved)

	if cfg.DefaultTTLMinutes, err = intEnv("DEFAULT_TTL_MINUTES", 60); err != nil {
		return cfg, err
	}
	if cfg.ReaperIntervalSeconds, err = intEnv("REAPER_INTERVAL_SECONDS", 30); err != nil {
		return cfg, err
	}
	if cfg.DockerPortBase, err = intEnv("DOCKER_PORT_BASE", 10000); err != nil {
		return cfg, err
	}
	if cfg.BandwidthCapMbit, err = intEnv("BANDWIDTH_CAP_MBIT", 200); err != nil {
		return cfg, err
	}

	return cfg, nil
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
