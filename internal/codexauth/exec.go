package codexauth

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"sync"
)

// execRunner drives the real Codex CLI.
//
// The login command is started and left running, with its output collected on a
// pipe so the caller can read the authorization code while the process is still
// blocked waiting for a human.
type execRunner struct{}

// codexEnv points the CLI at the Ceraa data directory, so credentials land with
// the rest of Ceraa's state instead of the process's home.
func codexEnv() []string {
	env := os.Environ()
	home := os.Getenv("HOME")
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = home + "/.codex"
	}
	return append(env, "CODEX_HOME="+codexHome, "HOME="+home)
}

func (execRunner) StartDeviceAuth(ctx context.Context) (loginProcess, error) {
	if _, err := exec.LookPath(codexBinary); err != nil {
		return nil, errNoCodexBinary
	}
	cmd := exec.CommandContext(ctx, codexBinary, "login", "--device-auth")
	cmd.Env = codexEnv()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	process := &execLogin{cmd: cmd}
	process.collect(stdout)
	process.collect(stderr)
	go func() { _ = cmd.Wait() }()
	return process, nil
}

func (execRunner) LoginStatus(ctx context.Context) (string, error) {
	if _, err := exec.LookPath(codexBinary); err != nil {
		return "", errNoCodexBinary
	}
	cmd := exec.CommandContext(ctx, codexBinary, "login", "status")
	cmd.Env = codexEnv()
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// execLogin is a running login command whose output is accumulated in the
// background, so a partial read never blocks the caller.
type execLogin struct {
	cmd *exec.Cmd

	mu  sync.Mutex
	buf bytes.Buffer
}

func (e *execLogin) collect(r interface{ Read([]byte) (int, error) }) {
	go func() {
		chunk := make([]byte, 4096)
		for {
			n, err := r.Read(chunk)
			if n > 0 {
				e.mu.Lock()
				e.buf.Write(chunk[:n])
				e.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
}

func (e *execLogin) Output() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.buf.String()
}

func (e *execLogin) Wait() error {
	if e.cmd.ProcessState != nil {
		return nil
	}
	return e.cmd.Wait()
}

func (e *execLogin) Kill() error {
	if e.cmd.Process == nil {
		return nil
	}
	return e.cmd.Process.Kill()
}
