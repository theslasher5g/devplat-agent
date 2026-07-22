package api_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/theslasher5g/devplat-agent/internal/api"
	"github.com/theslasher5g/devplat-agent/internal/config"
	"github.com/theslasher5g/devplat-agent/internal/vmmanager"
)

// fakeBackend mirrors vmmanager's own test double: no netlink/iptables/
// Firecracker, just fills in what Boot promises to fill in.
type fakeBackend struct{}

func (fakeBackend) Boot(_ context.Context, vm *vmmanager.VM, nc vmmanager.NetConfig, _ string) error {
	vm.DockerEndpoint = nc.HostIP.String()
	vm.Pid = 12345
	return nil
}
func (fakeBackend) Stop(context.Context, *vmmanager.VM) error { return nil }

// newTestManager builds a real Manager whose slot-derived guest IPs land in
// 127/8 — on Linux the entire block is loopback, so a test can actually
// LISTEN on the derived "guest" address (slot 0 → 127.0.0.2) and exercise
// the proxy's real dial+hijack+pipe path end to end, no VM required.
func newTestManager(t *testing.T) (*vmmanager.Manager, *vmmanager.VM) {
	t.Helper()
	golden := t.TempDir() + "/golden.ext4"
	if err := os.WriteFile(golden, []byte("fake rootfs"), 0o644); err != nil {
		t.Fatalf("write golden image: %v", err)
	}
	cfg := config.Config{
		VMStateDir:        t.TempDir(),
		GoldenImagePath:   golden,
		DefaultTTLMinutes: 60,
		TapIPBase:         net.ParseIP("127.0.0.0").To4(),
		DockerPortBase:    10000,
		WireguardCIDR:     "10.10.0.0/24",
	}
	m, err := vmmanager.New(cfg, fakeBackend{})
	if err != nil {
		t.Fatalf("vmmanager.New: %v", err)
	}
	vm, err := m.Create(context.Background(), "team_test", 60, 1, 64)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return m, vm
}

const testToken = "test-agent-token"

func upgradeRequest(vmID string, port int, token string) string {
	return fmt.Sprintf("GET /vms/%s/proxy/%d HTTP/1.1\r\nHost: agent\r\nAuthorization: Bearer %s\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n", vmID, port, token)
}

func TestProxyPort_PipesBothWays(t *testing.T) {
	m, vm := newTestManager(t)

	// "Guest" server on the slot-derived guest IP. Writes a greeting the
	// moment a connection lands (server-first bytes, like a Postgres error
	// or SSH banner would) and then echoes one line back.
	guestLn, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skipf("cannot bind 127.0.0.2 (non-Linux loopback?): %v", err)
	}
	defer guestLn.Close()
	guestPort := guestLn.Addr().(*net.TCPAddr).Port
	go func() {
		conn, err := guestLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("hello-from-guest\n"))
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte("echo:" + line))
	}()

	srv := httptest.NewServer(api.NewServer(m, testToken, ""))
	defer srv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("dial test server: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := conn.Write([]byte(upgradeRequest(vm.ID, guestPort, testToken))); err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("expected 101, got %d", resp.StatusCode)
	}

	greeting, err := br.ReadString('\n')
	if err != nil || greeting != "hello-from-guest\n" {
		t.Fatalf("expected guest greeting, got %q (err %v)", greeting, err)
	}
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatalf("write through proxy: %v", err)
	}
	echoed, err := br.ReadString('\n')
	if err != nil || echoed != "echo:ping\n" {
		t.Fatalf("expected echo, got %q (err %v)", echoed, err)
	}
}

func TestProxyPort_Errors(t *testing.T) {
	m, vm := newTestManager(t)
	srv := httptest.NewServer(api.NewServer(m, testToken, ""))
	defer srv.Close()

	get := func(path, token string) int {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if got := get(fmt.Sprintf("/vms/%s/proxy/80", vm.ID), "wrong-token"); got != http.StatusUnauthorized {
		t.Errorf("bad token: expected 401, got %d", got)
	}
	if got := get(fmt.Sprintf("/vms/%s/proxy/0", vm.ID), testToken); got != http.StatusBadRequest {
		t.Errorf("port 0: expected 400, got %d", got)
	}
	if got := get(fmt.Sprintf("/vms/%s/proxy/70000", vm.ID), testToken); got != http.StatusBadRequest {
		t.Errorf("port 70000: expected 400, got %d", got)
	}
	if got := get("/vms/vm_does_not_exist/proxy/80", testToken); got != http.StatusNotFound {
		t.Errorf("unknown vm: expected 404, got %d", got)
	}
}
