// Package procfile records a running Ceraa process so another invocation can
// find and stop it, and knows when it has genuinely exited.
package procfile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Record identifies one Ceraa process.
//
// StartTicks is the kernel's process start time in clock ticks since boot. It is
// carried so a stale record can be told apart from a live process that happens to
// have inherited the PID: a PID on its own is not an identity, because PIDs are
// reused and a stop aimed at a recycled PID lands on an unrelated program.
type Record struct {
	PID        int    `json:"pid"`
	PGID       int    `json:"pgid"`
	StartTicks uint64 `json:"start_ticks"`
	Command    string `json:"command"`
}

// Path returns the record path inside a data directory.
func Path(dataDir string) string { return filepath.Join(dataDir, "ceraa.pid") }

// Write records the current process and refuses to overwrite a live record, so
// two daemons cannot both believe they own the data directory.
func Write(path string, rec Record) error {
	// Refuse only when a *different* live process holds the slot. Comparing the
	// recorded start time rather than the pid is what lets a daemon rewrite its
	// own record, and what stops a recycled pid from being mistaken for a
	// running instance. A record whose process is gone is stale and is replaced.
	if existing, err := Read(path); err == nil && existing.StartTicks != rec.StartTicks {
		if matches, matchErr := Matches(existing); matchErr == nil && matches {
			return fmt.Errorf("ceraa is already running as pid %d", existing.PID)
		}
	}
	payload := fmt.Sprintf("{\"pid\":%d,\"pgid\":%d,\"start_ticks\":%d,\"command\":%q}\n",
		rec.PID, rec.PGID, rec.StartTicks, rec.Command)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(payload), 0o600); err != nil {
		return fmt.Errorf("write pid file: %w", err)
	}
	// Rename rather than write in place: a stop that reads a half-written file
	// would see a truncated PID and signal the wrong process.
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("commit pid file: %w", err)
	}
	return nil
}

// Read loads a record.
func Read(path string) (Record, error) {
	var rec Record
	raw, err := os.ReadFile(path)
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return rec, fmt.Errorf("pid file %s is malformed: %w", path, err)
	}
	if rec.PID <= 0 {
		return rec, fmt.Errorf("pid file %s is malformed: no pid", path)
	}
	return rec, nil
}

// Remove deletes the record, ignoring an already-absent file.
func Remove(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Probe reports whether a PID exists and whether it is a zombie.
//
// The distinction is the whole point of this package. A process that has exited
// but not yet been reaped still answers kill(0) successfully, so a caller that
// polls with kill(0) concludes a dead process is still running and escalates to
// SIGKILL against a corpse. That is not a cosmetic bug: it makes every restart
// script look like the daemon refuses to stop, and hides the real behaviour,
// which is that the daemon exits immediately and correctly.
func Probe(pid int) (alive bool, zombie bool, err error) {
	state, _, err := statFields(pid)
	if err != nil {
		return false, false, err
	}
	return true, state == "Z", nil
}

// statFields returns the process state and start time from /proc/<pid>/stat.
//
// The comm field is parenthesised and may itself contain spaces and parentheses,
// so the fields after it are located from the last ')' rather than by splitting
// the whole line, which would shift every index whenever a process is named
// something like "my (weird) name".
func statFields(pid int) (state string, startTicks uint64, err error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", 0, err
	}
	line := string(raw)
	close := strings.LastIndex(line, ")")
	if close < 0 {
		return "", 0, fmt.Errorf("cannot parse stat for pid %d", pid)
	}
	rest := strings.Fields(line[close+1:])
	// rest[0] is state (field 3); starttime is field 22, so index 19 in rest.
	if len(rest) < 20 {
		return "", 0, fmt.Errorf("short stat for pid %d", pid)
	}
	startTicks, err = strconv.ParseUint(rest[19], 10, 64)
	if err != nil {
		return rest[0], 0, fmt.Errorf("parse start time for pid %d: %w", pid, err)
	}
	return rest[0], startTicks, nil
}

// StartTicks returns the kernel start time used to detect PID reuse.
func StartTicks(pid int) (uint64, error) {
	_, ticks, err := statFields(pid)
	return ticks, err
}

// GroupIsolated reports whether the process leads its own process group, which is
// true when it was started with setsid. Only then is it safe to signal the
// negative PID: a process that inherited its shell's group would take the shell
// down with it.
func GroupIsolated(rec Record) bool { return rec.PGID > 0 && rec.PGID == rec.PID }

// Outcome describes how a stop finished.
type Outcome string

const (
	// StoppedCleanly means the process exited after SIGTERM, which is the normal
	// path once a shutdown handler is installed.
	StoppedCleanly Outcome = "stopped"
	// StoppedKilled means SIGTERM was ignored or too slow and SIGKILL was needed.
	StoppedKilled Outcome = "killed"
	// AlreadyGone means nothing was running at the recorded identity.
	AlreadyGone Outcome = "already stopped"
)

// Terminate signals a recorded process and waits for it to actually exit.
//
// Signals the whole process group when the target leads its own group. Chromium
// is launched as a child of Ceraa, so terminating only the Ceraa PID leaves the
// browser tree running: it gets reparented to init and survives indefinitely.
// Observed here as 23 orphaned Chromium processes outliving 21 restarts.
func Terminate(rec Record, grace time.Duration) (Outcome, error) {
	matches, err := Matches(rec)
	if err != nil || !matches {
		return AlreadyGone, nil
	}
	target := rec.PID
	if GroupIsolated(rec) {
		target = -rec.PID
	}
	if err := syscall.Kill(target, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return AlreadyGone, fmt.Errorf("send SIGTERM: %w", err)
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		// Poll by identity, not by kill(0): a reaped process and a zombie both
		// still exist, and treating either as running is what makes a clean exit
		// look like a refusal to exit.
		if alive, _, err := Probe(rec.PID); err != nil || !alive {
			return StoppedCleanly, nil
		}
		if matches, err := Matches(rec); err != nil || !matches {
			return StoppedCleanly, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Still there. Escalate.
	if target < 0 {
		syscall.Kill(target, syscall.SIGKILL)
	} else {
		syscall.Kill(rec.PID, syscall.SIGKILL)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if alive, _, err := Probe(rec.PID); err != nil || !alive {
			return StoppedKilled, nil
		}
		if matches, err := Matches(rec); err != nil || !matches {
			return StoppedKilled, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return StoppedKilled, fmt.Errorf("pid %d survived SIGKILL", rec.PID)
}

// Matches reports whether the recorded process is still the process that was
// recorded, guarding against acting on a recycled PID.
func Matches(rec Record) (bool, error) {
	_, zombie, err := Probe(rec.PID)
	if err != nil {
		return false, err
	}
	if zombie {
		// The original process is gone; the entry is a leftover corpse awaiting
		// reaping and is not a live target.
		return false, nil
	}
	ticks, err := StartTicks(rec.PID)
	if err != nil {
		return false, err
	}
	if rec.StartTicks != 0 && ticks != rec.StartTicks {
		return false, nil
	}
	return true, nil
}
