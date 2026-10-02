package miniapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeClient writes a stand-in for the tunnel client. It is a shell script so
// that the tests below drive the real seam: a process started, its output
// read from both streams, and an origin parsed out of a line the way the real
// client prints one. Driving a stub instead would pass while the parsing, the
// stream wiring or the reap were all wrong.
func fakeClient(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in is a shell script")
	}
	path := filepath.Join(t.TempDir(), "cloudflared")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write stand-in: %v", err)
	}
	return path
}

// quietTunnel turns the log line a failure would write into a recording, so a
// test can assert what was said instead of only that something was.
func quietTunnel(t *testing.T) *[]string {
	t.Helper()
	var said []string
	previousWarn := warn
	warn = func(message string) { said = append(said, message) }
	t.Cleanup(func() { warn = previousWarn })
	return &said
}

func TestTunnelReadsTheOriginTheClientAnnounces(t *testing.T) {
	// The banner is printed first, by an older client's habit of advertising
	// its terms before it has anything to advertise. A parser that takes the
	// first https link in the output returns that page instead, and returns it
	// in a fraction of a second, so it is part of this fixture rather than a
	// separate case: the sequence is what was wrong.
	client := fakeClient(t, `
printf '%s\n' "2026-10-02T09:41:29Z INF Thank you for trying Cloudflare Tunnel. These account-less Tunnels are subject to the Cloudflare Online Services Terms of Use (https://www.cloudflare.com/website-terms/), and Cloudflare reserves the right to investigate your use of Tunnels."
printf '%s\n' "2026-10-02T08:32:08Z INF Requesting new quick Tunnel on trycloudflare.com..."
printf '%s\n' "+--------------------------------------------------------------+"
printf '%s\n' "|  Your quick Tunnel has been created! Visit it at (it may take some time to be reachable):  |"
printf '%s\n' "|  https://metric-refinance-lady-buck.trycloudflare.com                                       |"
printf '%s\n' "+--------------------------------------------------------------+"
sleep 30
`)
	tunnel, err := startTunnel(context.Background(), client, "http://127.0.0.1:0")
	if err != nil {
		t.Fatalf("startTunnel: %v", err)
	}
	defer tunnel.Close()

	want := "https://metric-refinance-lady-buck.trycloudflare.com"
	if got := tunnel.Origin(); got != want {
		t.Fatalf("origin = %q, want %q", got, want)
	}
}

// The client repeats the address in its request log once it is serving, always
// with a path on the end. Taking that one would report a URL a browser cannot
// open, so the first announcement has to be the one that wins.
func TestTunnelKeepsTheAnnouncementOverTheRequestLog(t *testing.T) {
	client := fakeClient(t, `
printf '%s\n' "|  https://plain-refinance-oval-fox.trycloudflare.com  |"
printf '%s\n' "ERR Request failed dest=https://plain-refinance-oval-fox.trycloudflare.com/events"
sleep 30
`)
	tunnel, err := startTunnel(context.Background(), client, "http://127.0.0.1:0")
	if err != nil {
		t.Fatalf("startTunnel: %v", err)
	}
	defer tunnel.Close()

	want := "https://plain-refinance-oval-fox.trycloudflare.com"
	if got := tunnel.Origin(); got != want {
		t.Fatalf("origin = %q, want %q", got, want)
	}
}

// A client that fails rather than announces must say so, and the reason has to
// reach the log. The answer a caller is handed is one sentence, so the lines
// behind it are the part that would otherwise be lost.
func TestTunnelReportsAClientThatExitsWithoutAnnouncing(t *testing.T) {
	said := quietTunnel(t)
	client := fakeClient(t, `echo 'ERR protocol negotiation failed'
exit 3
`)
	if _, err := startTunnel(context.Background(), client, "http://127.0.0.1:0"); err == nil {
		t.Fatal("startTunnel succeeded for a client that never announced an address")
	}
	joined := strings.Join(*said, "\n")
	if !strings.Contains(joined, "protocol negotiation failed") {
		t.Fatalf("logged %q, want the client's own reason for failing", joined)
	}
}

// The override has to win, or a test pointing the seam at a script would
// download the real client instead, and an operator who named a binary would
// have their decision overruled by a fetch they did not ask for.
func TestTheTunnelOverrideBeatsProvisioning(t *testing.T) {
	t.Setenv("CERAA_CLOUDFLARED", "/definitely/not/a/client")
	command, provisioning := tunnelCommand()
	if command != "/definitely/not/a/client" {
		t.Errorf("command = %q, want the override", command)
	}
	if provisioning != "" {
		t.Errorf("provisioning = %q, want nothing reported when an override was given", provisioning)
	}
}

// A client that could not be provisioned is the common case on a machine that
// has never shared a tunnel and has no network, and the reply costs a round
// trip if it does not say what to do about it.
func TestStartTunnelReportsWhyThereIsNoClient(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	if err := server.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer server.Close()

	previous := resolveTunnelClient
	resolveTunnelClient = func() (string, string) {
		return "", "could not provision cloudflared (404); install it yourself or set CERAA_CLOUDFLARED"
	}
	t.Cleanup(func() { resolveTunnelClient = previous })

	_, err := server.StartTunnel()
	if err == nil {
		t.Fatal("StartTunnel succeeded with no client to run")
	}
	if !strings.Contains(err.Error(), "CERAA_CLOUDFLARED") {
		t.Errorf("err = %q, want it to name the setting that fills the gap", err)
	}
}

// Close has to take the child with it. A tunnel left running after the surface
// is gone is a process holding an origin that forwards to a port nothing is
// listening on, which fails as a connection reset the user has to diagnose.
func TestTunnelCloseEndsTheClient(t *testing.T) {
	client := fakeClient(t, `
printf '%s\n' "|  https://quiet-hill-tan-moth.trycloudflare.com  |"
sleep 60
`)
	tunnel, err := startTunnel(context.Background(), client, "http://127.0.0.1:0")
	if err != nil {
		t.Fatalf("startTunnel: %v", err)
	}

	closed := make(chan struct{})
	go func() {
		tunnel.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end the client")
	}
}

// A surface that has been shut down must refuse rather than advertise a port
// it stopped accepting on. Its fields all still read back after close, so
// nothing else catches this.
func TestStartTunnelRefusesAfterTheSurfaceIsClosed(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	if err := server.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := server.StartTunnel(); err == nil {
		t.Fatal("StartTunnel succeeded on a surface that has been closed")
	}
}

// StartTunnel is the only thing that creates a tunnel, so the endpoint and the
// chat command both read the same value off it. This asserts they agree, and
// that a surface nobody has shared reports the absence rather than a guess.
func TestTheTunnelEndpointReportsWhatStartTunnelAdvertised(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	initData := initDataFor(t, testToken, testUser, 0)

	read := func() string {
		t.Helper()
		rec := do(t, server.Handler(), http.MethodGet, "/api/tunnel", initData)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/tunnel = %d, want 200", rec.Code)
		}
		var body struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body.URL
	}

	if got := read(); got != "" {
		t.Fatalf("url = %q, want empty before anything has been shared", got)
	}
	if got := server.TunnelAddress(); got != "" {
		t.Fatalf("TunnelAddress() = %q, want empty before anything has been shared", got)
	}

	// Standing in for a started child: the wiring under test is the field and
	// its two readers, and a real handshake is the previous test's subject.
	server.tunnel = &Tunnel{origin: "https://small-lake-red-owl.trycloudflare.com"}

	want := "https://small-lake-red-owl.trycloudflare.com"
	if got := read(); got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}
	if got := server.TunnelAddress(); got != want {
		t.Fatalf("TunnelAddress() = %q, want %q", got, want)
	}
}

// The endpoint carries the same admission gate as everything else on the
// surface. An address that tells a stranger where the page is, is a leak of
// exactly the kind the Handler exists to prevent.
func TestTheTunnelEndpointIsNotOpenToTheMachine(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tunnel", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/tunnel without a payload = %d, want 401", rec.Code)
	}
}

func TestTunnelOriginFrom(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		// The announcement, inside its box, with the padding after it.
		{"2026-10-02T08:32:13Z INF |  https://metric-refinance-lady-buck.trycloudflare.com                                      |", "https://metric-refinance-lady-buck.trycloudflare.com"},
		// The terms banner the client prints before it has anything to say.
		{"INF Thank you for trying Cloudflare Tunnel. Subject to the Cloudflare Online Services Terms of Use (https://www.cloudflare.com/website-terms/), and Cloudflare reserves the right.", ""},
		// A later request log line, with a path, must not be preferred.
		{"ERR Request failed dest=https://lucky-river-owl.trycloudflare.com/events event=0", "https://lucky-river-owl.trycloudflare.com"},
		// Two candidates on one line: the real one still wins.
		{"INF terms (https://www.cloudflare.com/website-terms/) now serving https://small-lake-red-fox.trycloudflare.com", "https://small-lake-red-fox.trycloudflare.com"},
		{"https://bare.example.test", ""},
		{"https://trycloudflare.com", ""},
		{"INF Requesting new quick Tunnel on trycloudflare.com...", ""},
		{"", ""},
		{"   ", ""},
		{"only http:// insecure.example.test", ""},
	} {
		if got := tunnelOriginFrom(tc.line); got != tc.want {
			t.Errorf("tunnelOriginFrom(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}

func TestTunnelEnabledFailsClosedOnATypo(t *testing.T) {
	for value, want := range map[string]bool{
		"":     false,
		"no":   false,
		"on":   true,
		"YES":  true,
		"True": true,
		"1":    true,
		"oui":  false,
	} {
		t.Setenv("CERAA_MINIAPP_TUNNEL", value)
		if got := TunnelEnabled(); got != want {
			t.Errorf("TunnelEnabled() with %q = %v, want %v", value, got, want)
		}
	}
}

// A client that has exited keeps its origin readable, and that hostname resolves
// nowhere. Trusting it is how a menu button ends up advertising something that
// answers Cloudflare 1033, so the dead state has to be distinguishable.
func TestAGoneTunnelReportsItselfGone(t *testing.T) {
	tunnel := &Tunnel{origin: "https://dead.trycloudflare.com", cancel: func() {}, done: make(chan struct{})}
	if tunnel.Gone() {
		t.Fatal("a tunnel whose client is running reports itself gone")
	}
	close(tunnel.done)
	if !tunnel.Gone() {
		t.Error("a tunnel whose client exited still reports itself alive")
	}
}

// StartTunnel hands back a recorded origin without checking whether anything is
// still serving it. EnsureTunnel is the variant that replaces a dead one, which
// is the whole reason it exists.
func TestEnsureTunnelReplacesAClientThatExited(t *testing.T) {
	srv, _, _ := newFixture(t, testUser)
	const dead = "https://dead.trycloudflare.com"
	stopped := &Tunnel{origin: dead, cancel: func() {}, done: make(chan struct{})}
	close(stopped.done)
	srv.tunnelMu.Lock()
	srv.tunnel = stopped
	srv.tunnelMu.Unlock()

	if !srv.TunnelGone() {
		t.Fatal("the surface does not report a tunnel that has exited")
	}
	// The trap, stated as a fact rather than a hope: the old entry point answers
	// with an origin nothing is serving. If this ever starts failing, the trap
	// has changed and EnsureTunnel may no longer be needed.
	if got, err := srv.StartTunnel(); err != nil || got != dead {
		t.Skipf("StartTunnel no longer answers with the recorded origin (got %q, %v), so the "+
			"reason EnsureTunnel exists has changed", got, err)
	}
	// The fix: EnsureTunnel must not hand that origin back. Opening a replacement
	// is expected to fail here, because there is no tunnel client in a test, and
	// failing is the correct answer. Returning the dead origin would not be.
	if got, err := srv.EnsureTunnel(); err == nil && got == dead {
		t.Fatalf("EnsureTunnel answered with the origin of a client that exited: %q", got)
	}
}
