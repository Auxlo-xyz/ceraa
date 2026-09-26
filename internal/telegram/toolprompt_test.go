package telegram

import (
	"strings"
	"testing"
)

// TestDescribeToolRequestNeverDumpsFileContent pins the fix for an unusable
// permission prompt. Arguments arrive as a map, so rendering them with fmt
// produced Go's map syntax and dumped an entire file into the chat.
func TestDescribeToolRequestNeverDumpsFileContent(t *testing.T) {
	content := "<!DOCTYPE html>\n<html><body>" + strings.Repeat("padding ", 500) + "</body></html>"
	got := describeToolRequest("write", map[string]any{
		"path":    "fashion-landing/index.html",
		"content": content,
	})

	if strings.Contains(got, "map[") {
		t.Fatalf("prompt still contains Go map syntax: %s", got)
	}
	if strings.Contains(got, "DOCTYPE") {
		t.Fatalf("prompt leaked the file body: %s", got)
	}
	if !strings.Contains(got, "fashion-landing/index.html") {
		t.Fatalf("prompt must name the target: %s", got)
	}
	if !strings.Contains(got, "chars") {
		t.Fatalf("prompt should report the payload size instead of the payload: %s", got)
	}
}

// TestDescribeToolRequestIdentifiesEachTool pins that the identifying argument is
// what gets shown, for the tools a human actually approves.
func TestDescribeToolRequestIdentifiesEachTool(t *testing.T) {
	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"bash", map[string]any{"command": "go test ./...", "workdir": "/tmp"}, "go test ./..."},
		{"read", map[string]any{"path": "go.mod", "offset": 1, "limit": 20}, "go.mod"},
		{"edit", map[string]any{"path": "main.go", "old_text": "a", "new_text": "b"}, "main.go"},
		{"grep", map[string]any{"pattern": "func main", "path": "."}, "func main"},
		{"webfetch", map[string]any{"url": "https://example.com/a?b=c"}, "https://example.com/a?b=c"},
	}
	for _, tc := range cases {
		got := describeToolRequest(tc.tool, tc.args)
		if !strings.Contains(got, tc.want) {
			t.Errorf("describeToolRequest(%q) = %q, want it to contain %q", tc.tool, got, tc.want)
		}
		if strings.Contains(got, "map[") {
			t.Errorf("describeToolRequest(%q) leaked map syntax: %q", tc.tool, got)
		}
	}
}

// TestDescribeToolRequestTruncates pins that a long command is bounded so the
// message stays readable.
func TestDescribeToolRequestTruncates(t *testing.T) {
	long := strings.Repeat("echo hello && ", 200)
	got := describeToolRequest("bash", map[string]any{"command": long})
	if len([]rune(got)) > 420 {
		t.Fatalf("prompt not bounded: %d runes", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated value should be marked: %q", got)
	}
}

// TestDescribeToolRequestHandlesUnknownTool pins that an unrecognised tool still
// renders every argument rather than nothing.
func TestDescribeToolRequestHandlesUnknownTool(t *testing.T) {
	got := describeToolRequest("some_new_tool", map[string]any{"alpha": "one", "beta": float64(2)})
	if !strings.Contains(got, "alpha: one") || !strings.Contains(got, "beta: 2") {
		t.Fatalf("unknown tool prompt = %q", got)
	}
}

// TestDescribeToolRequestHandlesNonMapArguments pins the degenerate inputs.
func TestDescribeToolRequestHandlesNonMapArguments(t *testing.T) {
	if got := describeToolRequest("bash", nil); got != "no arguments" {
		t.Fatalf("nil args = %q", got)
	}
	if got := describeToolRequest("bash", "just a string"); got != "input: just a string" {
		t.Fatalf("string args = %q", got)
	}
	if got := describeToolRequest("unknown", map[string]string{"k": "v"}); !strings.Contains(got, "k: v") {
		t.Fatalf("string map args = %q", got)
	}
}

// TestSummariseToolRequestQuotesEveryLine pins that each rendered line is quoted,
// so the chat client does not reflow a path or command.
func TestSummariseToolRequestQuotesEveryLine(t *testing.T) {
	got := summariseToolRequest("bash", map[string]any{"command": "make build"})
	if got != "`command: make build`" {
		t.Fatalf("summary = %q", got)
	}
}
