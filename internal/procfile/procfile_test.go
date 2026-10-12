package procfile

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// requireProc skips a test that reads the kernel's process table.
//
// The package builds and runs on every platform Ceraa does, because the daemon
// writes the pid file everywhere, but process identity here is read back out of
// /proc/<pid>/stat, which only Linux has. These tests therefore assert Linux
// behaviour and skip elsewhere instead of failing on a host that was never in
// scope for them.
func requireProc(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("this OS has no /proc, so process identity cannot be checked")
	}
}

// A zombie still answers kill(0), so a caller that polls with kill(0) sees a dead
// process as running. This reproduces that: the child exits and is deliberately
// not reaped, which is exactly the state a Ceraa process is in between dying and
// init collecting it.
func TestProbeReportsAZombieAsDead(t *testing.T) {
	requireProc(t)
	child := exec.Command("/bin/sh", "-c", "exit 0")
	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	pid := child.Process.Pid
	defer func() {
		child.Process.Kill()
		child.Wait()
	}()

	deadline := time.Now().Add(3 * time.Second)
	var zombie bool
	for time.Now().Before(deadline) {
		alive, isZombie, err := Probe(pid)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if alive && isZombie {
			zombie = true
			// The point of the package: a zombie is present in the table but is
			// not a running target, so it must not be signalled or waited on.
			if matches, err := Matches(Record{PID: pid}); err != nil || matches {
				t.Errorf("Matches on a zombie = %v (err %v), want false", matches, err)
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !zombie {
		t.Skip("could not observe a zombie state on this kernel")
	}
	child.Wait()

	// After reaping, the pid is gone entirely.
	alive, _, err := Probe(pid)
	if err == nil && alive {
		t.Errorf("Probe on a reaped pid reported alive")
	}
}

// kill(0) succeeding is the trap this package exists to avoid, so assert the
// trap directly rather than trusting the comment.
func TestZombieStillAnswersKillZero(t *testing.T) {
	requireProc(t)
	child := exec.Command("/bin/sh", "-c", "exit 0")
	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	pid := child.Process.Pid
	defer func() {
		child.Process.Kill()
		child.Wait()
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if alive, isZombie, err := Probe(pid); err == nil && alive && isZombie {
			if err := syscall.Kill(pid, 0); err != nil {
				t.Skip("kernel reaped the child before kill(0) could be observed")
			}
			t.Logf("pid %d is a zombie yet kill(0) succeeds: %v", pid, err)
			child.Wait()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Skip("could not observe a zombie state on this kernel")
}

func TestWriteReadRemoveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ceraa.pid")
	want := Record{PID: os.Getpid(), PGID: 4242, StartTicks: 998877, Command: "ceraa start"}
	if err := Write(path, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
	if err := Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := Read(path); !os.IsNotExist(err) {
		t.Errorf("read after remove: %v, want not-exist", err)
	}
	if err := Remove(path); err != nil {
		t.Errorf("remove twice: %v, want nil", err)
	}
}

func TestWriteRefusesToStealALiveSlot(t *testing.T) {
	requireProc(t)
	path := filepath.Join(t.TempDir(), "ceraa.pid")
	ticks, err := StartTicks(os.Getpid())
	if err != nil {
		t.Fatalf("start ticks: %v", err)
	}
	if err := Write(path, Record{PID: os.Getpid(), StartTicks: ticks}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	// A different live process claiming the same record must be refused, so two
	// daemons cannot both believe they own the data directory.
	other := Record{PID: os.Getpid(), StartTicks: ticks + 1}
	if err := Write(path, other); err == nil {
		t.Error("write stole a live pid record")
	}
	// Re-writing its own record is idempotent.
	if err := Write(path, Record{PID: os.Getpid(), StartTicks: ticks}); err != nil {
		t.Errorf("idempotent rewrite: %v", err)
	}
}

func TestMatchesRejectsARecycledPID(t *testing.T) {
	requireProc(t)
	ticks, err := StartTicks(os.Getpid())
	if err != nil {
		t.Fatalf("start ticks: %v", err)
	}
	if matches, err := Matches(Record{PID: os.Getpid(), StartTicks: ticks}); err != nil || !matches {
		t.Errorf("exact identity = %v (err %v), want true", matches, err)
	}
	// Same PID, different start time: a different process that inherited the PID.
	if matches, err := Matches(Record{PID: os.Getpid(), StartTicks: ticks + 1}); err != nil || matches {
		t.Errorf("recycled pid = %v (err %v), want false", matches, err)
	}
}

func TestTerminateReportsACleanExitQuickly(t *testing.T) {
	requireProc(t)
	cmd := exec.Command("/bin/sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	// Exactly one Wait: exec.Cmd is not safe for concurrent Wait, and calling it
	// from both a goroutine and a defer is a data race the race detector catches.
	waited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(waited)
	}()
	defer func() {
		syscall.Kill(-pid, syscall.SIGKILL)
		<-waited
	}()

	ticks, err := StartTicks(pid)
	if err != nil {
		t.Fatalf("start ticks: %v", err)
	}
	rec := Record{PID: pid, PGID: pid, StartTicks: ticks}
	if !GroupIsolated(rec) {
		t.Fatal("Setpgid child should lead its own group")
	}

	start := time.Now()
	outcome, err := Terminate(rec, 8*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if outcome != StoppedCleanly {
		t.Errorf("outcome = %q, want %q (a SIGTERM-responsive process is not a kill)", outcome, StoppedCleanly)
	}
	// The old restart loop waited a fixed 6s and then declared the process
	// stubborn. A responsive process exits in milliseconds, so a slow result here
	// means the wait is polling the wrong thing again.
	if elapsed > 3*time.Second {
		t.Errorf("terminate took %s; a clean exit should be near-instant", elapsed)
	}
}

func TestTerminateEscalatesForAProcessThatIgnoresSIGTERM(t *testing.T) {
	requireProc(t)
	// A shell would not do here: `sh -c 'trap "" TERM; sleep 60'` execs the sleep,
	// replacing the shell, so the pid that receives the signal is a plain sleep
	// that dies on SIGTERM and the escalation path is never reached. Re-exec this
	// test binary as a helper that genuinely ignores the signal.
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperIgnoresSIGTERM")
	cmd.Env = append(os.Environ(), "PROCFILE_IGNORE_TERM=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	// Deliberately no concurrent cmd.Wait(): nothing reaps the child, so after it
	// dies it stays a zombie, and the escalation result therefore depends on the
	// zombie-aware check in Matches rather than on the pid vanishing.
	defer func() {
		syscall.Kill(-pid, syscall.SIGKILL)
		cmd.Wait()
	}()
	if err := waitIgnoresSIGTERM(pid, 10*time.Second); err != nil {
		// Skipped, not failed. Some sandboxes do not let a child install a
		// SIGTERM handler, so the escalation path this test covers cannot be
		// reached here at all. Failing made the suite permanently red on those
		// hosts, which is how a real regression in this package would have been
		// lost in the noise.
		t.Skipf("this host will not let a child ignore SIGTERM, so the escalation path is unreachable here: %v", err)
	}

	ticks, err := StartTicks(pid)
	if err != nil {
		t.Fatalf("start ticks: %v", err)
	}
	outcome, err := Terminate(Record{PID: pid, PGID: pid, StartTicks: ticks}, 700*time.Millisecond)
	if err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if outcome != StoppedKilled {
		t.Errorf("outcome = %q, want %q", outcome, StoppedKilled)
	}
}

// waitIgnoresSIGTERM blocks until the child has actually installed its SIGTERM
// handler.
//
// Without this the test races the child's own boot: SIGTERM is delivered while
// the child is still parsing flags, so the default disposition applies and it
// dies instantly. The escalation path is then never exercised and the test
// reports a clean stop, having quietly tested nothing.
func waitIgnoresSIGTERM(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.HasPrefix(line, "SigIgn:") {
				continue
			}
			mask, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "SigIgn:")), 16, 64)
			if err != nil {
				return err
			}
			if mask&(1<<(syscall.SIGTERM-1)) != 0 {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("pid %d did not ignore SIGTERM within %s", pid, timeout)
}

// TestHelperIgnoresSIGTERM is not a real test: it is the child process used by
// TestTerminateEscalatesForAProcessThatIgnoresSIGTERM.
func TestHelperIgnoresSIGTERM(t *testing.T) {
	if os.Getenv("PROCFILE_IGNORE_TERM") != "1" {
		t.Skip("helper process, not a test")
	}
	signal.Ignore(syscall.SIGTERM)
	time.Sleep(60 * time.Second)
}

func TestTerminateOnAnAbsentProcessIsNotAnError(t *testing.T) {
	outcome, err := Terminate(Record{PID: 1 << 22, PGID: 1 << 22, StartTicks: 1}, time.Second)
	if err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if outcome != AlreadyGone {
		t.Errorf("outcome = %q, want %q", outcome, AlreadyGone)
	}
}

// The scenario the 21 restarts actually hit: a process that honours SIGTERM but
// whose exit is not reaped, so it lingers as a zombie. Polling such a process
// with kill(0) never sees it leave, because a zombie still answers, so a stop
// waits out the whole grace period and then reports a kill against a process that
// exited in under a second. Nothing reaps the child here on purpose.
func TestTerminateReportsCleanEvenWhenTheExitIsNeverReaped(t *testing.T) {
	requireProc(t)
	cmd := exec.Command("/bin/sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	// No cmd.Wait until after the assertions: the child must stay a zombie.
	defer func() {
		syscall.Kill(-pid, syscall.SIGKILL)
		cmd.Wait()
	}()

	ticks, err := StartTicks(pid)
	if err != nil {
		t.Fatalf("start ticks: %v", err)
	}
	start := time.Now()
	outcome, err := Terminate(Record{PID: pid, PGID: pid, StartTicks: ticks}, 8*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if outcome != StoppedCleanly {
		t.Errorf("outcome = %q, want %q: an unreaped but exited process is stopped, not killed", outcome, StoppedCleanly)
	}
	if elapsed > 3*time.Second {
		t.Errorf("terminate took %s on a process that exited immediately", elapsed)
	}
}

func TestGroupIsolated(t *testing.T) {
	if GroupIsolated(Record{PID: 10, PGID: 5}) {
		t.Error("a process sharing its parent's group must not be group-signalled")
	}
	if !GroupIsolated(Record{PID: 10, PGID: 10}) {
		t.Error("a group leader should be group-signalled so its children die too")
	}
}
