package primitives

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// PrimitiveResult is a unit of output from a primitive.
type PrimitiveResult struct {
	Content    string
	Error      string
	OutputPath string
}

// RunShell executes a bash command with bounded output.
func RunShell(command string, workDir string, timeout time.Duration) (PrimitiveResult, error) {
	return RunShellContext(context.Background(), command, workDir, timeout)
}

func RunShellContext(parent context.Context, command string, workDir string, timeout time.Duration) (PrimitiveResult, error) {
	return runShell(parent, command, workDir, timeout, nil)
}

// RunShellContextWithEnv runs a command with extra environment entries.
//
// The entries are set on the child process only, so a value passed this way never
// appears in the command line — which means it cannot be read out of `ps` — and
// never enters this process's own environment.
func RunShellContextWithEnv(parent context.Context, command string, workDir string, timeout time.Duration, extraEnv []string) (PrimitiveResult, error) {
	return runShell(parent, command, workDir, timeout, extraEnv)
}

func runShell(parent context.Context, command string, workDir string, timeout time.Duration, extraEnv []string) (PrimitiveResult, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	if workDir != "" {
		cmd.Dir = workDir
	}
	output := newBoundedOutput(4096, 4096)
	var file *os.File
	var fileErr error
	file, fileErr = os.CreateTemp("", "ceraa-shell-*.log")
	if fileErr == nil {
		defer file.Close()
	}
	var writer io.Writer = output
	if file != nil {
		writer = io.MultiWriter(file, output)
	}
	cmd.Stdout = writer
	cmd.Stderr = writer
	err := cmd.Run()
	result := PrimitiveResult{Content: output.String(), OutputPath: filePath(file)}
	if err != nil {
		result.Error = explainMissingBinary(output.String(), err.Error())
	}
	if ctx.Err() != nil {
		result.Error = ctx.Err().Error()
	}
	return result, err
}

type boundedOutput struct {
	head      []byte
	tail      []byte
	total     int
	headLimit int
	tailLimit int
}

func newBoundedOutput(headLimit, tailLimit int) *boundedOutput {
	return &boundedOutput{headLimit: headLimit, tailLimit: tailLimit}
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	b.total += len(data)
	remaining := b.headLimit - len(b.head)
	if remaining > 0 {
		if len(data) <= remaining {
			b.head = append(b.head, data...)
		} else {
			b.head = append(b.head, data[:remaining]...)
		}
	}
	b.tail = append(b.tail, data...)
	if len(b.tail) > b.tailLimit {
		b.tail = append([]byte(nil), b.tail[len(b.tail)-b.tailLimit:]...)
	}
	return len(data), nil
}

func (b *boundedOutput) String() string {
	if b.total <= b.headLimit {
		return string(b.head)
	}
	var out strings.Builder
	out.Write(b.head)
	out.WriteString("\n...[output truncated]...\n")
	out.Write(b.tail)
	return out.String()
}

func filePath(file *os.File) string {
	if file == nil {
		return ""
	}
	return file.Name()
}

// ReadFile reads a file from disk, limited to 5MB.
func ReadFile(path string) (PrimitiveResult, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return PrimitiveResult{Error: err.Error()}, err
	}
	if len(b) > 5*1024*1024 {
		return PrimitiveResult{Content: string(b[:5*1024*1024])}, nil
	}
	return PrimitiveResult{Content: string(b)}, nil
}

// GlobFiles lists directory entries via glob.
func GlobFiles(pattern string) (PrimitiveResult, error) {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return PrimitiveResult{Error: err.Error()}, err
	}
	return PrimitiveResult{Content: joinPaths(matches)}, nil
}

func joinPaths(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	result := paths[0]
	for _, p := range paths[1:] {
		result += "\n" + p
	}
	return result
}

// explainMissingBinary names the substitute when a command is not installed.
//
// A real session ran `rg` twice and got "command not found" both times before
// falling back to grep, so the cost of a binary being absent is two wasted round
// trips per session. Naming the equivalent turns the second one into a corrected
// call instead of a repeat.
//
// The hint goes in the error, not in the prompt. Whether a binary is installed is
// a property of the deployment, and putting it in the system prompt would put
// per-environment text in the cached prefix, which is where volatile content does
// the most damage.
func explainMissingBinary(output, errText string) string {
	if !strings.Contains(output, "command not found") {
		return errText
	}
	hint := ""
	switch {
	case strings.Contains(output, "rg: command not found"):
		hint = "ripgrep is not installed here; use grep -rn, or find for file names"
	case strings.Contains(output, "fd: command not found"):
		hint = "fd is not installed here; use find"
	case strings.Contains(output, "fd: No such file"):
		hint = "fd is not installed here; use find"
	case strings.Contains(output, "jq: command not found"):
		hint = "jq is not installed here; use grep, or a python3 -c one-liner for JSON"
	}
	if hint == "" {
		return errText
	}
	return errText + " - " + hint
}
