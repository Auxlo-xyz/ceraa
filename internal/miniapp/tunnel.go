package miniapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"ceraa/internal/envs"
	"ceraa/internal/provision"
)

// tunnelStartup is how long the client is given to announce an origin.
//
// A quick tunnel normally announces in a handful of seconds. The bound exists
// so a client that is installed but cannot reach the edge refuses instead of
// leaving a chat message unanswered forever, and it is generous rather than
// tight because a cold handshake on a slow link is the common case here, not a
// failure. Nothing waits on it during shutdown: the start hangs off the
// server's own context, so closing the surface ends the handshake at once.
const tunnelStartup = 45 * time.Second

// tunnelTail bounds what a failure puts in the log. The client's whole output
// is a wall of lines with a run id in every one; the last few are what a
// person can actually act on, and they are the ones that say why it stopped.
const tunnelTail = 4

// TunnelEnabled reports whether the public front was asked for at startup.
//
// It is a separate switch from Enabled because opening a loopback port and
// putting that port on the internet are different decisions, and only one of
// them publishes the surface. Fails closed for the same reason Enabled does.
func TunnelEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(envs.Get("MINIAPP_TUNNEL"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// tunnelCommand resolves the client that will carry the address, provisioning
// one when this machine has none.
//
// The override wins over provisioning, because an operator who named a binary
// has made a decision and a download that ignores it contradicts them. It is
// also how a test points the seam at a script instead of the real client.
//
// The second result is what provisioning did, empty when it did nothing worth
// saying. It is a path and a log line rather than an error, because the absence
// of a client is carried by the empty path and explained by that same line.
func tunnelCommand() (string, string) {
	if name := strings.TrimSpace(envs.Get("CLOUDFLARED")); name != "" {
		return name, ""
	}
	return provision.EnsureCloudflared()
}

// resolveTunnelClient is the seam between StartTunnel and provisioning: the
// real one fetches a client, and a test supplies an answer without downloading
// tens of megabytes to learn that a path is empty.
var resolveTunnelClient = tunnelCommand

// quickTunnelSuffix is the domain a quick tunnel is published under. It is
// part of the contract rather than a shortcut: a quick tunnel is the only mode
// this ever runs, and an address on any other domain is not an address we
// asked for.
const quickTunnelSuffix = ".trycloudflare.com"

// tunnelOriginFrom pulls the origin out of one line of the client's output.
//
// The acceptance test is the domain, not merely the presence of an https link,
// because the first thing the client prints is a terms banner carrying two
// links of its own. Taking the first https anywhere in the output advertised
// cloudflare.com's terms page as the surface's address, in 222ms and long
// before any tunnel existed, and the page that came back was theirs rather
// than ours.
//
// The announcement is printed inside a drawn box, so the line carries a border
// and a run of padding after the address; cutting at the first delimiter
// handles that, and every candidate on the line is tested rather than only the
// first.
func tunnelOriginFrom(line string) string {
	const marker = "https://"
	for _, part := range strings.Split(line, marker)[1:] {
		end := strings.IndexAny(part, " \t\"'`),;|")
		if end == 0 {
			continue
		}
		if end < 0 {
			end = len(part)
		}
		host := hostOf(marker + part[:end])
		if len(host) <= len(quickTunnelSuffix) {
			continue
		}
		if !strings.EqualFold(host[len(host)-len(quickTunnelSuffix):], quickTunnelSuffix) {
			continue
		}
		// The host and not the candidate, because a line the client logs once
		// it is serving carries the address with a path on the end of it.
		return marker + host
	}
	return ""
}

// hostOf reduces an address to its host, dropping any path it carries and a
// trailing dot a resolver would treat as the same name.
func hostOf(origin string) string {
	host := strings.TrimPrefix(origin, "https://")
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	return strings.TrimSuffix(host, ".")
}

// lineSink turns the child's two streams into lines.
//
// Both point at one sink on purpose: which of them carries the announcement is
// a detail of the client's version, and guessing wrong means waiting for an
// address that arrived on the stream nobody is reading. The two copying
// goroutines are serialised against each other, so a line is never torn in
// half by a write arriving from the other stream.
type lineSink struct {
	mu   sync.Mutex
	buf  []byte
	tail []string
	line func(string)
}

func (s *lineSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			break
		}
		text := strings.TrimRight(string(s.buf[:i]), "\r")
		s.buf = s.buf[i+1:]
		s.tail = append(s.tail, text)
		if len(s.tail) > tunnelTail {
			s.tail = s.tail[len(s.tail)-tunnelTail:]
		}
		if s.line != nil {
			s.line(text)
		}
	}
	return len(p), nil
}

// report is the handful of lines a failure carries, not the whole log.
func (s *lineSink) report() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.tail, " | ")
}

// Tunnel is one running client and the origin it announced.
type Tunnel struct {
	origin string
	cancel context.CancelFunc
	done   chan struct{}
}

// Origin is where the surface is reachable from outside this machine.
func (t *Tunnel) Origin() string { return t.origin }

// Gone reports whether the client has exited.
//
// The origin is a hostname the other end chose, and nothing on this side makes it
// stop resolving: the address keeps reading back perfectly well after the process
// that was serving it is gone. A caller that trusts Origin alone therefore
// advertises a dead address forever, which is how a menu button ends up pointing
// at something that answers Cloudflare's 1033.
func (t *Tunnel) Gone() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// Close ends the client and waits for the child to be gone, so a shutdown
// never leaves a process holding an origin nobody is watching any more.
func (t *Tunnel) Close() {
	t.cancel()
	<-t.done
}

// startTunnel runs the client against address and returns once it has said
// where it put it.
//
// The wait is the whole point. The origin is handed out by the other end and
// is not known here until it prints, so answering before the announcement
// would mean handing out a guess. base cancels a handshake that is still in
// flight, which is how shutdown avoids waiting out tunnelStartup.
func startTunnel(base context.Context, command, address string) (*Tunnel, error) {
	// Defensive: the reason a client is missing is carried by the caller that
	// resolved it, and reaching here without one means that contract broke.
	if command == "" {
		return nil, errors.New("no tunnel client to run")
	}
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	cmd := exec.CommandContext(ctx, command, "tunnel", "--url", address, "--no-autoupdate")

	sink := &lineSink{}
	found := make(chan string, 1)
	sink.line = func(text string) {
		origin := tunnelOriginFrom(text)
		if origin == "" {
			return
		}
		// The first announcement wins. The client repeats the address in the
		// request log later, with a path on the end of it, and taking that one
		// would report a URL that a browser cannot open.
		select {
		case found <- origin:
		default:
		}
	}
	cmd.Stdout = sink
	cmd.Stderr = sink

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start %s: %w", command, err)
	}
	// Started, so the child has to be reaped whether or not this wait
	// succeeds. Leaving it unreaped is how a tunnel outlives the surface that
	// asked for it.
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	timer := time.NewTimer(tunnelStartup)
	defer timer.Stop()
	select {
	case origin := <-found:
		return &Tunnel{origin: origin, cancel: cancel, done: done}, nil
	case <-ctx.Done():
		cancel()
		<-done
		return nil, errors.New("the surface closed before the tunnel announced an address")
	case <-done:
		cancel()
		reportTunnel(sink)
		return nil, errors.New("the tunnel client exited before announcing an address")
	case <-timer.C:
		cancel()
		<-done
		reportTunnel(sink)
		return nil, errors.New("the tunnel client announced no address in time")
	}
}

// reportTunnel puts what the client actually said into the log. The answer a
// caller is handed stays to one sentence; the lines behind it are the ones
// that explain it, and they belong somewhere with room for them.
func reportTunnel(sink *lineSink) {
	if tail := sink.report(); tail != "" {
		warn("Mini App tunnel client said: " + tail)
	}
}
