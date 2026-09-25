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
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	if workDir != "" {
		cmd.Dir = workDir
	}
	output := newBoundedOutput(4096, 4096)
	var file *os.File
	var fileErr error
	file, fileErr = os.CreateTemp("", "sooraa-shell-*.log")
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
		result.Error = err.Error()
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
