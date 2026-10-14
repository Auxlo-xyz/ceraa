package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The body half of Zen's free-tier contract. Checked through Stream rather than
// against the helper alone, because the flag, the tools the transport builds and
// the rewrite have to agree on one payload, and a request with no tools at all
// (a compaction) is the case that fails when they do not.
func TestOpenCodeGatewayShapesTheStreamBody(t *testing.T) {
	cases := map[string]struct {
		flag bool
		// the tools the transport would have built before the rewrite ran
		tools []ToolDef
		want  []string
	}{
		"flag on, compaction sends no tools": {
			flag: true, tools: nil,
			want: []string{"bash", "read"},
		},
		"flag on, chat carries the registry": {
			flag: true,
			tools: []ToolDef{
				{Name: "bash", Description: "run", Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{"command": map[string]any{"type": "string"}},
					"required":   []string{"command"},
				}},
				{Name: "read", Description: "open", Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
					"required":   []string{"path"},
				}},
				{Name: "edit", Description: "change", Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{"filePath": map[string]any{"type": "string"}},
					"required":   []string{"filePath"},
				}},
			},
			want: []string{"bash", "read", "edit"},
		},
		"flag on, a real tool set missing bash": {
			flag: true,
			tools: []ToolDef{{Name: "read", Description: "open", Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required":   []string{"path"},
			}}},
			want: []string{"bash", "read"},
		},
		"flag off, nothing is added": {
			flag: false, tools: nil,
			want: nil,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
			}))
			defer server.Close()

			client := New(ProviderConfig{
				BaseURL:         server.URL,
				Model:           "mimo-v2.6-flash-free",
				APIKey:          "zk-1",
				OpenCodeGateway: tc.flag,
			})
			if err := client.Stream(t.Context(), StreamRequest{
				Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
				Tools:    tc.tools,
			}, func(ProviderResponse) {}); err != nil {
				t.Fatalf("Stream: %v", err)
			}

			// Every request streams. It is the first thing the gateway reads and
			// a non-streaming body is refused even when the rest is right.
			if body["stream"] != true {
				t.Errorf("stream = %v, want true", body["stream"])
			}
			got := map[string]bool{}
			if raw, ok := body["tools"].([]any); ok {
				for _, entry := range raw {
					tool, _ := entry.(map[string]any)
					function, _ := tool["function"].(map[string]any)
					name, _ := function["name"].(string)
					got[name] = true
				}
			}
			if tc.want == nil {
				if len(got) != 0 {
					t.Errorf("tools = %v, want none when the gateway is not in play", got)
				}
				return
			}
			for _, want := range tc.want {
				if !got[want] {
					t.Errorf("tools = %v, missing %q", got, want)
				}
			}
			// The pair only exists to satisfy the gateway when the caller named
			// no tools. "none" used to keep the model from calling stubs that end
			// nowhere, but the gateway now refuses "none", so the key must be
			// absent: a verification ping never executes an answer, and a real
			// run executes genuine bash/read tools anyway.
			hadTools := len(tc.tools) > 0
			if _, present := body["tool_choice"]; !hadTools && present {
				t.Errorf("tool_choice is present on a request with no real tools; the gateway refuses \"none\" and absent passes")
			}
			if hadTools && body["tool_choice"] == nil {
				t.Error("tool_choice was cleared from a request that had tools")
			}
			// Injected or carried, every bash/read the gateway sees must carry
			// its marker sentence with a coherent schema: that is the half of
			// the contract this rewrite owns.
			if raw, ok := body["tools"].([]any); ok {
				for _, entry := range raw {
					tool, _ := entry.(map[string]any)
					function, _ := tool["function"].(map[string]any)
					name, _ := function["name"].(string)
					marker := OpenCodeMarkerFor(name)
					if marker == "" {
						continue
					}
					desc, _ := function["description"].(string)
					if !strings.Contains(desc, marker) {
						t.Errorf("tool %q description %q lacks the gateway marker", name, desc)
					}
					params, _ := function["parameters"].(map[string]any)
					props, _ := params["properties"].(map[string]any)
					var required []string
					switch r := params["required"].(type) {
					case []any:
						for _, e := range r {
							if s, ok := e.(string); ok {
								required = append(required, s)
							}
						}
					case []string:
						required = r
					}
					for _, r := range required {
						if _, ok := props[r]; !ok {
							t.Errorf("tool %q requires %q, which is not in its properties", name, r)
						}
					}
					if len(required) == 0 {
						t.Errorf("tool %q carries no required array; the gateway refuses it", name)
					}
				}
			}
		})
	}
}

// OpenCodeTrafficTools is a fixed, externally defined pair, not whatever this
// program happens to name its registry. Renaming the Go tools is fine; losing
// the gateway's names is not, and only one of the two would be a subtle failure.
func TestOpenCodeTrafficToolsAreTheGatesExactNames(t *testing.T) {
	want := map[string]bool{"bash": true, "read": true}
	if len(OpenCodeTrafficTools) != len(want) {
		t.Fatalf("OpenCodeTrafficTools = %v, want exactly bash and read", OpenCodeTrafficTools)
	}
	for _, name := range OpenCodeTrafficTools {
		if !want[name] {
			t.Errorf("OpenCodeTrafficTools contains %q, which the gateway does not look for", name)
		}
	}
}

// The marker sentences are opencode's words pinned byte-exact, measured live
// on 2026-10-14 by bisecting each checked tool's description down to the
// sentence whose removal refuses the request. If upstream rewords a tool the
// marker moves with it: this test fails loudly at commit time rather than the
// provider dying quietly at runtime with a FreeTierError that reads like a
// key problem.
func TestOpenCodeMarkersArePinnedByteExact(t *testing.T) {
	want := map[string]string{
		"bash":  "IMPORTANT: This tool is for terminal operations like git, npm, docker, etc.",
		"edit":  "Performs exact string replacements in files.",
		"glob":  "- Fast file pattern matching tool that works with any codebase size",
		"grep":  "- Fast content search tool that works with any codebase size",
		"read":  "Read a file or directory from the local filesystem. If the path does not exist, an error is returned.",
		"write": "Writes a file to the local filesystem.",
	}
	for name, sentence := range want {
		if got := OpenCodeMarkerFor(name); got != sentence {
			t.Errorf("OpenCodeMarkerFor(%q) = %q, want the measured %q", name, got, sentence)
		}
	}
	for _, name := range []string{"", "bashx", "read_file", "edit_line", "task", "webfetch"} {
		if got := OpenCodeMarkerFor(name); got != "" {
			t.Errorf("OpenCodeMarkerFor(%q) = %q, want no marker for an unchecked name", name, got)
		}
	}
}
