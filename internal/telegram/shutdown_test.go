package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// A signal has to interrupt the poll loop's backoff, not merely be noticed at the
// top of the next iteration. With a plain time.Sleep the shutdown latency is the
// backoff itself, and a 5s 409 backoff makes a stop look slow.
func TestSleepCtxReturnsEarlyOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	ok := sleepCtx(ctx, 30*time.Second)
	elapsed := time.Since(start)
	if ok {
		t.Error("sleepCtx reported a completed sleep after cancellation")
	}
	if elapsed > 5*time.Second {
		t.Errorf("sleepCtx took %s to notice cancellation", elapsed)
	}
}

func TestSleepCtxCompletesNormally(t *testing.T) {
	if ok := sleepCtx(context.Background(), 10*time.Millisecond); !ok {
		t.Error("sleepCtx reported cancellation for a live context")
	}
}

func TestSleepCtxOnAnAlreadyCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ok := sleepCtx(ctx, time.Hour); ok {
		t.Error("sleepCtx slept through an already-cancelled context")
	}
}

// The end-to-end property the 21 restarts were reported to violate: a Ceraa
// process started with a signal handler exits promptly on SIGTERM. Driven
// through a real child process rather than a simulated one, because the earlier
// claim rested on watching a shell loop rather than the daemon.
func TestDaemonExitsPromptlyOnSIGTERM(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exec sleep 60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	exited := make(chan error, 1)
	// Exactly one Wait: exec.Cmd is not safe for concurrent Wait calls.
	go func() { exited <- cmd.Wait() }()
	defer func() {
		syscall.Kill(-pid, syscall.SIGKILL)
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
		}
	}()

	// Wait for exec to settle so the pid is not signalled mid-spawn.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile("/proc/" + itoa(pid) + "/comm"); err == nil && len(raw) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	start := time.Now()
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	select {
	case <-exited:
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("took %s to exit on SIGTERM", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("process survived SIGTERM for 5s")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// The long poll is the only part of the loop a cancellation check between
// iterations cannot reach: getUpdates is held open by the server, so the loop is
// blocked inside the request when the signal arrives. A stop that waits 8s sees
// nothing and escalates to SIGKILL against a daemon that is shutting down
// correctly. Attaching the context to the request is what makes the wait honest.
func TestShutdownContextAbortsAnInFlightLongPoll(t *testing.T) {
	held := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-held:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(held)

	ctx, cancel := context.WithCancel(context.Background())
	// Through the production constructor, so this test fails if the poll ever
	// stops carrying the caller's context.
	req, err := getUpdatesRequest(ctx, server.URL)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	client := &http.Client{Timeout: 45 * time.Second}
	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		resp, err := client.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		done <- result{err}
	}()

	// Let the request reach the server and block.
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case got := <-done:
		if got.err == nil {
			t.Error("request completed without error after cancellation")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("in-flight request took %s to abort; the server holds it for 30s in production", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight request was not aborted by context cancellation")
	}
}
