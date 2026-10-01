package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
				{Name: "bash", Description: "run"},
				{Name: "read", Description: "open"},
				{Name: "edit", Description: "change"},
			},
			want: []string{"bash", "read", "edit"},
		},
		"flag on, a real tool set missing bash": {
			flag:  true,
			tools: []ToolDef{{Name: "read", Description: "open"}},
			want:  []string{"bash", "read"},
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
			// no tools, and nothing should be calling a stub that ends nowhere.
			hadTools := len(tc.tools) > 0
			if choice, _ := body["tool_choice"].(string); !hadTools && choice != "none" {
				t.Errorf("tool_choice = %q, want none for a request with no real tools", choice)
			}
			if hadTools && body["tool_choice"] == nil {
				t.Error("tool_choice was cleared from a request that had tools")
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
