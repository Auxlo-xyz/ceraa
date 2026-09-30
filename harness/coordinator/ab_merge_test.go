package coordinator

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ceraa/harness/llm"
	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
	"ceraa/internal/minimaxauth"
	"ceraa/internal/paths"
	"ceraa/internal/secret"
)

// A/B for merging edit_line into batch_edit_file.
//
// The case for the merge is arithmetic: edit_line's definition costs 159 tokens
// per turn and it is the same operation as a one-element batch. The case against
// is behavioural: forcing array ceremony on every single edit may cause
// malformed calls, and one retry costs far more than 159 tokens.
//
// Arithmetic does not settle that, so this measures it. The decision metric is
// first-call success, because a first call that works needs no retry and a first
// call that fails does. The token counts come from the provider's own usage, not
// from a chars/4 estimate.
//
// Live-only: it spends plan credits and reads the real credential store.
func abDisabled() bool { return os.Getenv("CERAA_LIVE_AB") != "1" }

// abTask is one single-change edit, which is the entire risk surface: merging
// only affects how a lone change is expressed.
//
// want is the text the file must contain afterwards, so a call that parses and
// translates but edits the wrong thing still counts as a failure.
type abTask struct {
	name    string
	content string
	prompt  string
	want    string
}

// Varied on purpose. A merge decision resting on one prompt is a decision
// resting on one lucky sample, and the interesting failures are shape-specific:
// a value change, an insertion, a rename and a deletion each stress the wrapper
// differently.
func abTasks() []abTask {
	return []abTask{
		{
			name:    "value",
			content: "DEBUG = False\nTIMEOUT = 30\n",
			prompt:  "In the file config.py, set DEBUG to True.",
			want:    "DEBUG = True",
		},
		{
			name:    "numeric",
			content: "TIMEOUT = 30\nRETRIES = 3\n",
			prompt:  "In the file config.py, change TIMEOUT to 60.",
			want:    "TIMEOUT = 60",
		},
		{
			name:    "string",
			content: "NAME = \"old-name\"\n",
			prompt:  "In the file config.py, change the NAME string to \"new-name\".",
			want:    "NAME = \"new-name\"",
		},
		{
			name:    "rename",
			content: "def handle():\n    return 1\n",
			prompt:  "In the file app.py, rename the function handle to process.",
			want:    "def process()",
		},
		{
			name:    "delete",
			content: "DEBUG = False\nVERBOSE = True\n",
			prompt:  "In the file config.py, delete the line that sets VERBOSE to True.",
			want:    "VERBOSE",
		},
		{
			name:    "insert",
			content: "import os\n\nDEBUG = False\n",
			prompt:  "In the file app.py, add the line \"import sys\" directly below \"import os\".",
			want:    "import os\nimport sys",
		},
	}
}

// abVariant is one arm of the test: a name, a registry, and the definition size
// actually sent to the provider.
type abVariant struct {
	name string
	reg  *tool.Registry
}

func abVariants() []abVariant {
	both := tool.DefaultRegistryForUser("x", nil, nil)
	merged := tool.DefaultRegistryForUser("x", nil, nil)
	// The whole proposal in one line: delete the duplicate definition.
	merged.Remove("edit_line")
	return []abVariant{
		{name: "both", reg: both},
		{name: "merged", reg: merged},
	}
}

func abDefinitionBytes(reg *tool.Registry) int {
	total := 0
	for _, tl := range reg.All() {
		d, _ := json.Marshal(llm.ToolDef{Name: tl.Name(), Description: tl.Description(), Parameters: tl.Schema()})
		total += len(d)
	}
	return total
}

// abCall is what one trial produced.
type abCall struct {
	toolName    string
	arguments   string
	parsed      map[string]any
	parseOK     bool
	translateOK bool
	appliedOK   bool
	fileOK      bool
	inputTok    int
	outputTok   int
	prose       bool
}

// abHarvestUsage pulls the provider's own token counts out of whichever event
// carries them.
//
// Read from the response rather than estimated from the payload: the whole
// question is what the definitions cost, and an estimate that is wrong in the
// direction that favours the change is worthless.
func abHarvestUsage(ev map[string]any, call *abCall) {
	usage, ok := ev["usage"].(map[string]any)
	if !ok {
		// `found`, not `ok`: a redeclared ok here would shadow the outer one, the
		// guard below would still read the outer false, and every message_start
		// usage would be discarded. That reports input_tokens as 0, which makes
		// the definitions look free.
		if msg, found := ev["message"].(map[string]any); found {
			usage, ok = msg["usage"].(map[string]any)
		}
	}
	if !ok {
		return
	}
	if v, ok := usage["input_tokens"].(float64); ok && int(v) > call.inputTok {
		call.inputTok = int(v)
	}
	// Cumulative in the Messages shape, so the largest seen is the total rather
	// than the last delta overwriting it.
	if v, ok := usage["output_tokens"].(float64); ok && int(v) > call.outputTok {
		call.outputTok = int(v)
	}
}

func TestLiveEditToolMergeAversusBoth(t *testing.T) {
	if abDisabled() {
		t.Skip("live A/B disabled; set CERAA_LIVE_AB=1 with a populated credential store")
	}
	dir := paths.DataDir()
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatal(err)
	}
	// Resolved once here so a missing credential skips with a clear reason
	// before any request is built; abToken resolves per request, since a token
	// that expires mid-run must not be reused.
	if _, err := minimaxauth.Resolve(context.Background(), minimaxauth.NewStore(minimaxauth.RegionGlobal)); err != nil {
		t.Skipf("no usable credential: %v", err)
	}

	repeats := 3
	if v := os.Getenv("CERAA_LIVE_AB_REPEATS"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &repeats); err != nil {
			t.Fatalf("CERAA_LIVE_AB_REPEATS=%q is not a number", v)
		}
	}

	// The definition cost, straight from what gets sent.
	for _, v := range abVariants() {
		t.Logf("ARM %-7s definitions %5d bytes ~%4d tokens/turn", v.name, abDefinitionBytes(v.reg), abDefinitionBytes(v.reg)/4)
	}

	summary := map[string]*abArm{}
	for _, v := range abVariants() {
		summary[v.name] = &abArm{name: v.name}
	}

	for _, v := range abVariants() {
		for _, task := range abTasks() {
			for r := 0; r < repeats; r++ {
				call := abTrial(t, v.reg, task)
				summary[v.name].add(task.name, call)
			}
		}
	}

	t.Log("")
	for _, arm := range []*abArm{summary["both"], summary["merged"]} {
		arm.report(t)
	}
	if both, merged := summary["both"], summary["merged"]; both != nil && merged != nil {
		t.Log("")
		t.Logf("DECISION INPUT: both first-call success %.0f%% vs merged %.0f%% over %d trials each",
			both.successRate(), merged.successRate(), both.totals)
		t.Logf("DECISION INPUT: definition cost %d vs %d bytes per request (~%d vs ~%d tokens/turn)",
			abDefinitionBytes(abVariants()[0].reg), abDefinitionBytes(abVariants()[1].reg),
			abDefinitionBytes(abVariants()[0].reg)/4, abDefinitionBytes(abVariants()[1].reg)/4)
	}
}

type abArm struct {
	name   string
	totals int
	ok     int
	byTask map[string]struct{ total, ok, retries int }
	// usage is averaged over the arm, and reported separately from correctness
	// because a cheaper arm that fails more is not cheaper.
	inputTokSum  int
	outputTokSum int
	usageN       int
}

func (a *abArm) add(task string, c abCall) {
	if a.byTask == nil {
		a.byTask = map[string]struct{ total, ok, retries int }{}
	}
	a.totals++
	entry := a.byTask[task]
	entry.total++
	if c.parseOK && c.translateOK && c.appliedOK && c.fileOK {
		a.ok++
		entry.ok++
	}
	// A prose answer counts as a failure to act, not as an absent trial.
	if c.prose {
		entry.ok -= 1
		a.ok--
	}
	a.byTask[task] = entry
	if c.inputTok > 0 || c.outputTok > 0 {
		a.inputTokSum += c.inputTok
		a.outputTokSum += c.outputTok
		a.usageN++
	}
}

func (a *abArm) successRate() float64 {
	if a.totals == 0 {
		return 0
	}
	return float64(a.ok) / float64(a.totals) * 100
}

func (a *abArm) report(t *testing.T) {
	t.Helper()
	t.Logf("ARM %-7s first-call success %2d/%-2d (%.0f%%)", a.name, a.ok, a.totals, a.successRate())
	if a.usageN > 0 {
		t.Logf("           provider-reported usage: input %d, output %d (mean over %d calls)",
			a.inputTokSum/a.usageN, a.outputTokSum/a.usageN, a.usageN)
	}
	for name, e := range a.byTask {
		t.Logf("             %-8s %d/%d", name, e.ok, e.total)
	}
}

// abTrial runs one prompt against one arm and carries the resulting call all the
// way to the file, so "it worked" means the file changed, not that the JSON
// parsed.
func abTrial(t *testing.T, reg *tool.Registry, task abTask) abCall {
	t.Helper()
	call := abAsk(t, reg, task)

	work := t.TempDir()
	path := filepath.Join(work, "target.py")
	if err := os.WriteFile(path, []byte(task.content), 0o644); err != nil {
		t.Fatal(err)
	}

	streamed := llm.StreamedToolCall{ID: "ab1", Name: call.toolName, Arguments: call.arguments}
	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{
		reg:   reg,
		store: store,
		opMgr: operation.New(nil),
		cfg:   RunnerConfig{SessionID: "ab", WorkDir: work},
	}
	sess := &sessionstore.Session{ID: "ab", Version: 2}

	prepared, declined, _, err := runner.prepareTools(context.Background(), sess, "run1", []llm.StreamedToolCall{streamed})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) == 0 {
		// A declined call is exactly the shape failure a merge would cause, so it
		// is recorded rather than silently dropped.
		_ = declined
		return call
	}
	call.translateOK = true

	select {
	case result := <-runner.opMgr.Results():
		call.appliedOK = result.Status == operation.StatusCompleted
		if !call.appliedOK {
			t.Logf("  (%s) %s: %s", task.name, result.Status, result.Error)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("operation timed out")
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	call.fileOK = strings.Contains(string(content), task.want)
	if !call.fileOK {
		t.Logf("  (%s) wanted %q, file is now:\n%s", task.name, task.want, content)
	}
	return call
}

// abAsk makes the single provider request and returns the tool call it produced,
// along with the provider's own token counts.
func abAsk(t *testing.T, reg *tool.Registry, task abTask) abCall {
	t.Helper()
	call := abCall{}

	var defs []map[string]any
	for _, tl := range reg.All() {
		defs = append(defs, map[string]any{
			"name": tl.Name(), "description": tl.Description(), "input_schema": tl.Schema(),
		})
	}

	full := "Here is the file " + filepath.Base(filepath.Join("x", "target.py")) + ":\n\n```python\n" + task.content + "```\n\n" + task.prompt
	payload, _ := json.Marshal(map[string]any{
		"model": abModel(), "max_tokens": 800, "stream": true, "tools": defs,
		"messages": []map[string]any{{"role": "user", "content": full}},
	})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://agent.minimax.io/mavis/api/v1/llm/v1/messages", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Authorization", "Bearer "+abToken(t))

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

	var name string
	var arguments string
	done := false
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "data:"))
		if line == "" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		// Usage rides on message_start and message_delta, so every event is
		// checked rather than one being assumed.
		abHarvestUsage(ev, &call)
		if cb, ok := ev["content_block"].(map[string]any); ok && name == "" && cb["type"] == "tool_use" {
			name, _ = cb["name"].(string)
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
	_ = done
	if name == "" {
		call.prose = true
		return call
	}
	call.toolName = name
	call.arguments = arguments
	call.parseOK = json.Unmarshal([]byte(arguments), &call.parsed) == nil
	return call
}

// abModel names the model, defaulting to the one the existing live check uses so
// the two are comparable.
func abModel() string {
	if m := os.Getenv("CERAA_LIVE_AB_MODEL"); m != "" {
		return m
	}
	return "MiniMax-M3"
}

func abToken(t *testing.T) string {
	t.Helper()
	cred, err := minimaxauth.Resolve(context.Background(), minimaxauth.NewStore(minimaxauth.RegionGlobal))
	if err != nil {
		t.Skipf("credential went away mid-test: %v", err)
	}
	return cred.AccessToken
}
