package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ceraa/internal/minimaxauth"
	"ceraa/internal/paths"
	"ceraa/internal/secret"
)

// Live-only: it spends plan credits and reads the real credential store.
func liveEditDisabled() bool { return os.Getenv("CERAA_LIVE_EDIT") != "1" }

// The question a unit test cannot answer: given both tools and a task needing
// several changes in one file, does the model reach for batch_edit_file, and does
// it encode the edits array in a shape the tool will accept?
func TestLiveTheModelReachesForBatchEditFile(t *testing.T) {
	if liveEditDisabled() {
		t.Skip("live edit check disabled")
	}
	// The secret store is opened explicitly: the bot opens it at startup, and a
	// test process has to do the same or every read comes back empty.
	dir := paths.DataDir()
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatal(err)
	}
	// Through Resolve, not the raw stored token: the stored access token is the
	// pre-refresh one and the endpoint answers 401 for it, which is what the
	// running bot avoids by refreshing before use.
	cred, err := minimaxauth.Resolve(context.Background(), minimaxauth.NewStore(minimaxauth.RegionGlobal))
	if err != nil {
		t.Skipf("no usable credential: %v", err)
	}
	if cred.ExpiresAt.Before(time.Now()) {
		t.Logf("warning: the resolved token reports expiry %s, in the past", cred.ExpiresAt)
	}

	// input_schema, not parameters: the endpoint reads the Messages shape and
	// answers "function parameters is empty" for the chat one.
	reg := DefaultRegistryForUser("x", nil, nil)
	var defs []map[string]any
	for _, tl := range reg.All() {
		defs = append(defs, map[string]any{
			"name": tl.Name(), "description": tl.Description(), "input_schema": tl.Schema(),
		})
	}
	t.Logf("offered %d tools, including edit_line and batch_edit_file", len(defs))

	prompt := "Here is the file /srv/app/config.py:\n\n" +
		"```python\nDEBUG = False\nTIMEOUT = 30\nRETRIES = 3\n```\n\n" +
		"Make three changes to that file: set DEBUG to True, TIMEOUT to 60, and RETRIES to 5."

	payload, _ := json.Marshal(map[string]any{
		"model": "MiniMax-M3", "max_tokens": 900, "stream": true, "tools": defs,
		"messages": []map[string]any{{"role": "user", "content": prompt}},
	})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://agent.minimax.io/mavis/api/v1/llm/v1/messages", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		t.Fatalf("HTTP %d: %s", resp.StatusCode, string(raw))
	}

	// Accumulate every fragment until the block stops, not just the first: a
	// tool_use input arrives in pieces and stopping early reads as malformed JSON.
	var name, arguments string
	done := false
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() && !done {
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "data:"))
		if line == "" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if cb, ok := ev["content_block"].(map[string]any); ok && name == "" {
			if cb["type"] == "tool_use" {
				name, _ = cb["name"].(string)
			}
		}
		if d, ok := ev["delta"].(map[string]any); ok {
			if pj, ok := d["partial_json"].(string); ok {
				arguments += pj
			}
		}
		if ev["type"] == "content_block_stop" && name != "" {
			done = true
		}
	}
	if name == "" {
		t.Skip("the model answered in prose rather than calling a tool")
	}
	t.Logf("MODEL CHOSE: %s", name)
	t.Logf("arguments: %s", arguments)

	chosen, found := reg.Get(name)
	if !found || chosen == nil {
		t.Fatalf("the model called an unregistered tool: %s", name)
	}
	var parsed map[string]any
	if json.Unmarshal([]byte(arguments), &parsed) != nil {
		t.Fatalf("arguments are not a JSON object: %s", arguments)
	}
	spec, err := chosen.Translate(parsed)
	if err != nil {
		t.Fatalf("the tool rejected its own arguments: %v", err)
	}
	edits, _ := spec.Args["edits"].([]map[string]any)
	t.Logf("TRANSLATED OK: type=%s edits=%d", spec.Type, len(edits))
	for i, e := range edits {
		old, _ := e["old_text"].(string)
		nw, _ := e["new_text"].(string)
		t.Logf("  edit %d: %q -> %q", i, old, nw)
	}
	if len(edits) < 3 {
		t.Errorf("only %d edits arrived for a three-change task", len(edits))
	}
}
