package scan

// ports_test.go exercises the port scanner with an injected dialer, so no
// connection leaves the machine and every state can be produced on demand.
//
// The host used here is a documentation address (TEST-NET-3). It is not
// private, so the egress wall lets it through and the scan reaches the
// injected dialer, and it is never dialled for real.

import (
	"context"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// scanHost is the address the scans pretend to target.
const scanHost = "203.0.113.10"

// TestScanPortsClassifiesTheStates: a refusal is a closed port, a timeout is
// a filtered one, and a connection is an open one.
func TestScanPortsClassifiesTheStates(t *testing.T) {
	useScopeFor(t, scanHost)
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		switch {
		case strings.HasSuffix(addr, ":443"):
			client, server := net.Pipe()
			server.Close()
			return client, nil
		case strings.HasSuffix(addr, ":80"):
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
		default:
			return nil, context.DeadlineExceeded
		}
	}

	results, err := ScanPorts(context.Background(), PortsOptions{
		Host:  scanHost,
		Ports: []int{443, 80, 8080},
		Dial:  dial,
	})
	if err != nil {
		t.Fatalf("ScanPorts: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %+v, want three ports", results)
	}
	states := map[int]string{}
	services := map[int]string{}
	for _, result := range results {
		states[result.Port] = result.State
		services[result.Port] = result.Service
	}
	if states[443] != PortOpen {
		t.Errorf("443 = %q, want open", states[443])
	}
	if states[80] != PortClosed {
		t.Errorf("80 = %q, want closed", states[80])
	}
	if states[8080] != PortFiltered {
		t.Errorf("8080 = %q, want filtered", states[8080])
	}
	if services[443] != "https" || services[80] != "http" {
		t.Errorf("service names = %v", services)
	}
}

// TestScanPortsRefusesAPrivateHost: the wall holds for raw TCP exactly as
// it holds for HTTP, and the refusal names the address.
func TestScanPortsRefusesAPrivateHost(t *testing.T) {
	useScopeFor(t, "127.0.0.1")
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		t.Error("a refused host was dialled")
		return nil, nil
	}

	_, err := ScanPorts(context.Background(), PortsOptions{
		Host:  "127.0.0.1",
		Ports: []int{80},
		Dial:  dial,
	})
	if err == nil {
		t.Fatal("a loopback host was accepted")
	}
	if !strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("the refusal does not name the address: %v", err)
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("the refusal does not name the reason: %v", err)
	}
}

// TestScanPortsTakesTheCommonPortsByDefault: with no explicit list the
// scan takes the head of the common table.
func TestScanPortsTakesTheCommonPortsByDefault(t *testing.T) {
	useScopeFor(t, scanHost)
	var mu sync.Mutex
	var dialled []string
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dialled = append(dialled, addr)
		mu.Unlock()
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}

	results, err := ScanPorts(context.Background(), PortsOptions{
		Host: scanHost,
		TopN: 3,
		Dial: dial,
	})
	if err != nil {
		t.Fatalf("ScanPorts: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %+v, want three ports", results)
	}
	mu.Lock()
	got := append([]string(nil), dialled...)
	mu.Unlock()
	want := []string{scanHost + ":80", scanHost + ":443", scanHost + ":22"}
	for _, addr := range want {
		if !containsString(got, addr) {
			t.Errorf("the common port %s was not tried, got %v", addr, got)
		}
	}
}

// TestScanPortsStopsAtTheCeiling: the ceiling is on ports attempted.
func TestScanPortsStopsAtTheCeiling(t *testing.T) {
	useScopeFor(t, scanHost)
	var mu sync.Mutex
	attempts := 0
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		attempts++
		mu.Unlock()
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}

	result, err := ScanPortsDetailed(context.Background(), PortsOptions{
		Host:     scanHost,
		Ports:    []int{80, 443, 22, 21, 25},
		MaxPorts: 2,
		Dial:     dial,
	})
	if err != nil {
		t.Fatalf("ScanPorts: %v", err)
	}
	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != 2 {
		t.Errorf("the dialer saw %d attempts, want 2 (the ceiling)", got)
	}
	if result.Attempted != 2 {
		t.Errorf("Attempted = %d, want 2", result.Attempted)
	}
	if result.Skipped != 3 {
		t.Errorf("Skipped = %d, want 3 (the ports the ceiling left out)", result.Skipped)
	}
}

// TestScanPortsRefusesWhatItCannotDo: the host is checked before anything
// is dialled.
func TestScanPortsRefusesWhatItCannotDo(t *testing.T) {
	useScopeFor(t, scanHost)
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		t.Error("a refused scan was dialled")
		return nil, nil
	}

	for _, host := range []string{"", "http://example.com", "example.com/path", "exa mple.com"} {
		if _, err := ScanPorts(context.Background(), PortsOptions{Host: host, Ports: []int{80}, Dial: dial}); err == nil {
			t.Errorf("host %q was accepted", host)
		}
	}
	if _, err := ScanPorts(context.Background(), PortsOptions{Host: scanHost, Ports: []int{70000}, Dial: dial}); err == nil {
		t.Error("an out of range port was accepted")
	}
}

// TestCommonPortTableHasEnoughEntries: the default list is the top hundred
// common ports, and every row carries a name.
func TestCommonPortTableHasEnoughEntries(t *testing.T) {
	entries := commonPortEntries()
	if len(entries) < 100 {
		t.Fatalf("the common port table holds %d entries, want at least 100", len(entries))
	}
	seen := map[int]bool{}
	for _, entry := range entries {
		if entry.port < 1 || entry.port > 65535 {
			t.Errorf("port %d is out of range", entry.port)
		}
		if entry.name == "" {
			t.Errorf("port %d carries no service name", entry.port)
		}
		if seen[entry.port] {
			t.Errorf("port %d is listed twice", entry.port)
		}
		seen[entry.port] = true
	}
	if serviceName(443) != "https" {
		t.Errorf("serviceName(443) = %q, want https", serviceName(443))
	}
	if serviceName(1) == "" {
		t.Error("serviceName(1) is empty")
	}
}
