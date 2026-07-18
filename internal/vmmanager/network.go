package vmmanager

import (
	"encoding/binary"
	"fmt"
	"net"
	"os/exec"
	"strconv"

	"github.com/theslasher5g/devplat-agent/internal/config"
	"github.com/vishvananda/netlink"
)

// NetConfig is the fully-derived network plan for one VM slot. Every field
// is a pure function of (host's TapIPBase, slot index) — see deriveNetConfig
// — so it never needs to be persisted; it's recomputed identically on agent
// restart from the slot alone.
type NetConfig struct {
	Slot       int
	TapName    string // fc-tap-<slot>
	HostIP     net.IP // .1 of a per-slot /30 — the tap device's host-side address
	GuestIP    net.IP // .2 of the same /30 — configured inside the VM via kernel boot args
	Mask       net.IPMask
	DockerPort int // host port DNAT'd to GuestIP:2375, reachable only from WireguardCIDR
}

// deriveNetConfig computes the network plan for a slot. Each slot consumes a
// /30 (4 addresses: network, host, guest, broadcast) out of the host's /16
// pool, so a single host supports up to ~16000 slots this way — far beyond
// any realistic capacity, with room to spare for gaps.
func deriveNetConfig(cfg config.Config, slot int) NetConfig {
	base := binary.BigEndian.Uint32(cfg.TapIPBase.To4())
	block := base + uint32(slot)*4
	return NetConfig{
		Slot:       slot,
		TapName:    fmt.Sprintf("fc-tap-%d", slot),
		HostIP:     uint32ToIP(block + 1),
		GuestIP:    uint32ToIP(block + 2),
		Mask:       net.CIDRMask(30, 32),
		DockerPort: cfg.DockerPortBase + slot,
	}
}

func uint32ToIP(v uint32) net.IP {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, v)
	return ip
}

func setupTapDevice(nc NetConfig) error {
	attrs := netlink.NewLinkAttrs()
	attrs.Name = nc.TapName
	tap := &netlink.Tuntap{LinkAttrs: attrs, Mode: netlink.TUNTAP_MODE_TAP}
	if err := netlink.LinkAdd(tap); err != nil {
		return fmt.Errorf("create tap %s: %w", nc.TapName, err)
	}
	addr := &netlink.Addr{IPNet: &net.IPNet{IP: nc.HostIP, Mask: nc.Mask}}
	if err := netlink.AddrAdd(tap, addr); err != nil {
		_ = netlink.LinkDel(tap)
		return fmt.Errorf("assign %s to %s: %w", addr, nc.TapName, err)
	}
	if err := netlink.LinkSetUp(tap); err != nil {
		_ = netlink.LinkDel(tap)
		return fmt.Errorf("bring up %s: %w", nc.TapName, err)
	}
	return nil
}

func teardownTapDevice(nc NetConfig) error {
	link, err := netlink.LinkByName(nc.TapName)
	if err != nil {
		return nil // already gone
	}
	return netlink.LinkDel(link)
}

func runCmd(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w (%s)", name, args, err, out)
	}
	return nil
}

// setupFirewall wires the DNAT for this VM's Docker port and denies any
// other inbound traffic to it. iptables (not nftables) for now: it's what's
// preinstalled on stock Ubuntu Server and simplest to reason about for a
// first cut; the rule set is small enough that migrating to nftables later
// is a mechanical change, not a design change.
func setupFirewall(cfg config.Config, nc NetConfig) error {
	port := strconv.Itoa(nc.DockerPort)
	// The DNAT below only reaches the guest if the host actually forwards
	// packets between the WireGuard interface and the tap device. Some hosts
	// set this in sysctl.conf, but don't rely on it — a fresh host with
	// forwarding off silently black-holes every tunnel connection.
	if err := runCmd("sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		return fmt.Errorf("enable ip_forward: %w", err)
	}
	steps := [][]string{
		// Only the WireGuard subnet (i.e. the scheduler) may reach this
		// VM's Docker API, DNAT'd from the host port to the guest.
		{"-t", "nat", "-A", "PREROUTING", "-p", "tcp", "-s", cfg.WireguardCIDR,
			"--dport", port, "-j", "DNAT", "--to-destination", nc.GuestIP.String() + ":2375"},
		// Belt and suspenders: reject the same port from anywhere else, in
		// case the host also has another interface routed to it.
		{"-A", "INPUT", "-p", "tcp", "--dport", port, "!", "-s", cfg.WireguardCIDR, "-j", "DROP"},
		// Explicitly allow the control plane (WireGuard subnet) to reach this
		// VM. Required, not redundant: the host's FORWARD policy is DROP, so
		// without an ACCEPT the DNAT'd NEW connection falls straight through
		// to the drop and the tunnel times out. The rules below only DROP
		// unwanted sources and ACCEPT replies — none of them accepts the
		// wanted NEW connection itself, which is exactly what this does.
		{"-A", "FORWARD", "-s", cfg.WireguardCIDR, "-o", nc.TapName, "-j", "ACCEPT"},
		// No unsolicited inbound to the VM from outside the tunnel at all.
		{"-A", "FORWARD", "-o", nc.TapName, "-m", "state", "--state", "NEW",
			"!", "-s", cfg.WireguardCIDR, "-j", "DROP"},
		{"-A", "FORWARD", "-o", nc.TapName, "-m", "state", "--state", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
		// Outbound from the VM (registry pulls, whatever the test containers
		// need) is allowed; the bandwidth cap (setupBandwidthCap) and — once
		// added — an egress blocklist are the abuse controls here, not a
		// full allowlist firewall, per the first-cut scope for this build step.
		{"-A", "FORWARD", "-i", nc.TapName, "-j", "ACCEPT"},
	}
	for _, args := range steps {
		if err := runCmd("iptables", args...); err != nil {
			return err
		}
	}
	return nil
}

func teardownFirewall(cfg config.Config, nc NetConfig) error {
	port := strconv.Itoa(nc.DockerPort)
	// Same rules with -D (delete) instead of -A/-I; errors are ignored
	// individually since a partially-applied setup shouldn't block cleanup.
	steps := [][]string{
		{"-t", "nat", "-D", "PREROUTING", "-p", "tcp", "-s", cfg.WireguardCIDR,
			"--dport", port, "-j", "DNAT", "--to-destination", nc.GuestIP.String() + ":2375"},
		{"-D", "INPUT", "-p", "tcp", "--dport", port, "!", "-s", cfg.WireguardCIDR, "-j", "DROP"},
		{"-D", "FORWARD", "-s", cfg.WireguardCIDR, "-o", nc.TapName, "-j", "ACCEPT"},
		{"-D", "FORWARD", "-o", nc.TapName, "-m", "state", "--state", "NEW",
			"!", "-s", cfg.WireguardCIDR, "-j", "DROP"},
		{"-D", "FORWARD", "-o", nc.TapName, "-m", "state", "--state", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
		{"-D", "FORWARD", "-i", nc.TapName, "-j", "ACCEPT"},
	}
	var firstErr error
	for _, args := range steps {
		if err := runCmd("iptables", args...); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// setupBandwidthCap applies a simple token-bucket rate limit per VM. Torn
// down implicitly when the tap device is deleted.
func setupBandwidthCap(nc NetConfig, mbit int) error {
	if mbit <= 0 {
		return nil
	}
	return runCmd("tc", "qdisc", "add", "dev", nc.TapName, "root", "tbf",
		"rate", fmt.Sprintf("%dmbit", mbit), "burst", "32kbit", "latency", "400ms")
}
