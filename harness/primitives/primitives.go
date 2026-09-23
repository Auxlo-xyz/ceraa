package primitives

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// PrimitiveResult is a unit of output from a primitive.
type PrimitiveResult struct {
	Content string
	Error   string
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
	out, err := cmd.CombinedOutput()
	result := PrimitiveResult{}
	if len(out) > 4000 {
		result.Content = string(out[:4000])
	} else {
		result.Content = string(out)
	}
	if err != nil {
		result.Error = err.Error()
	}
	return result, err
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
