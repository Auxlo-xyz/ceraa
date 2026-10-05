package miniapp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The control surface is the dashboard's whole backend, so every route gets a
// test here: an untested route is a 503 or a wrong shape discovered from the
// page. Stubs stand in for the bot's stores; the wiring of the real functions
// is covered by the packages that own them.

func controlFixture(t *testing.T) *Server {
	t.Helper()
	server, _, _ := newFixture(t, testUser)
	cfg := server.cfg
	cfg.Meta = func() map[string]any {
		return map[string]any{"reasoning_levels": []string{"off", "max"}}
	}
	cfg.Providers = func() (string, []ProviderEntry, error) {
		return "anthropic", []ProviderEntry{{ID: "anthropic", Endpoint: "https://api.anthropic.com", Model: "m", HasKey: true}}, nil
	}
	cfg.SetActiveProvider = func(id string) error { return nil }
	cfg.SaveProvider = func(id string, profile map[string]any) error { return nil }
	cfg.DeleteProvider = func(id string) error { return nil }
	cfg.Subscriptions = func() ([]SubscriptionEntry, error) {
		return []SubscriptionEntry{{ID: "codex", Label: "Codex", SignedIn: true}}, nil
	}
	cfg.Catalog = func() (map[string]any, error) {
		return map[string]any{"models": 412}, nil
	}
	cfg.RefreshCatalog = func() (map[string]any, error) {
		return map[string]any{"models": 420, "refreshed": true}, nil
	}
	cfg.RunParams = func() map[string]any {
		return map[string]any{"reasoning": "high"}
	}
	cfg.SetRunParams = func(update map[string]any) error { return nil }
	cfg.Tools = func() ([]string, []ToolEntry, error) {
		return []string{"read"}, []ToolEntry{{Name: "read", Description: "Read things"}}, nil
	}
	cfg.SetToolAllowed = func(name string, allowed bool) error { return nil }
	cfg.Memories = func() ([]MemoryEntry, error) {
		return []MemoryEntry{{ID: "m1", Type: "preference", Content: "likes tea"}}, nil
	}
	cfg.DeleteMemory = func(id string) error { return nil }
	cfg.TriggerCompaction = func(sessionID string) (map[string]any, error) {
		return map[string]any{"session": sessionID, "compacted": true}, nil
	}
	cfg.Skills = func() ([]SkillProposal, error) {
		return []SkillProposal{{ID: "s1", Name: "lint", Status: "pending"}}, nil
	}
	cfg.DecideSkill = func(id string, approve bool) error { return nil }
	cfg.Schedules = func() ([]ScheduleJob, error) {
		return []ScheduleJob{{ID: "j1", Cron: "0 * * * *", Task: "report"}}, nil
	}
	cfg.AddSchedule = func(job map[string]any) (string, error) { return "j2", nil }
	cfg.DeleteSchedule = func(id string) error { return nil }
	cfg.Stats = func() (map[string]any, error) {
		return map[string]any{"totals": map[string]any{"runs": 3}}, nil
	}
	cfg.Files = func(rel string) ([]FileEntry, error) {
		return []FileEntry{{Name: "a.txt", Size: 3}}, nil
	}
	cfg.Status = func() (map[string]any, error) {
		return map[string]any{"version": "v9", "running": true}, nil
	}
	cfg.Snapshots = func() ([]SnapshotEntry, error) {
		return []SnapshotEntry{{ID: "h1", Label: "a.go", Time: "2026-01-01T00:00:00Z"}}, nil
	}
	cfg.Todos = func(sessionID string) ([]TodoEntry, error) {
		return []TodoEntry{{ID: "td-1", Text: "write tests", Done: true}}, nil
	}
	cfg.RenameSession = func(id, title string) error { return nil }
	cfg.DeleteSession = func(id string) error { return nil }
	cfg.Todos = func(sessionID string) ([]TodoEntry, error) {
		return []TodoEntry{{ID: "td-1", Text: "write tests", Done: true}}, nil
	}
	cfg.SetTodo = func(sessionID, content, status string) error { return nil }
	cfg.Projects = func() (map[string]any, error) {
		return map[string]any{"workspace": "/w", "current": "/w/a", "entries": []string{"a"}}, nil
	}
	cfg.SetProject = func(root string) error { return nil }
	return New(cfg)
}

func getJSON(t *testing.T, server *Server, target string) (int, map[string]any) {
	t.Helper()
	initData := initDataFor(t, testToken, testUser, 0)
	rec := do(t, server.Handler(), http.MethodGet, target, initData)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET %s returned non-JSON: %s", target, rec.Body.String())
	}
	return rec.Code, body
}

func TestControlReadsEverySurface(t *testing.T) {
	server := controlFixture(t)
	for target, key := range map[string]string{
		"/api/meta":      "meta",
		"/api/providers": "providers",
		"/api/runparams": "params",
		"/api/tools":     "inventory",
		"/api/memories":  "memories",
		"/api/skills":    "proposals",
		"/api/schedules": "jobs",
		"/api/stats":     "stats",
		"/api/files":     "files",
		"/api/status":    "status",
		"/api/catalog":   "catalog",
		"/api/sessions":  "sessions",
		"/api/snapshots": "snapshots",
		"/api/todos":     "todos",
	} {
		code, body := getJSON(t, server, target)
		if code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", target, code)
			continue
		}
		if _, ok := body[key]; !ok {
			t.Errorf("GET %s missing %q in %v", target, key, body)
		}
	}
}

func TestControlWritesRoundTrip(t *testing.T) {
	server := controlFixture(t)
	initData := initDataFor(t, testToken, testUser, 0)
	cases := []struct {
		method  string
		target  string
		payload string
	}{
		{http.MethodPost, "/api/providers", `{"active":"anthropic"}`},
		{http.MethodPost, "/api/providers/x", `{"endpoint":"https://x","model":"m"}`},
		{http.MethodDelete, "/api/providers/x", ``},
		{http.MethodPost, "/api/runparams", `{"params":{"reasoning":"low"}}`},
		{http.MethodPost, "/api/tools", `{"name":"read","allowed":true}`},
		{http.MethodDelete, "/api/memories/m1", ``},
		{http.MethodPost, "/api/compaction", `{"session_id":"s"}`},
		{http.MethodPost, "/api/skills/s1", `{"decision":"approve"}`},
		{http.MethodPost, "/api/skills/s1", `{"decision":"bogus"}`},
		{http.MethodPost, "/api/schedules", `{"cron":"0 * * * *","task":"report"}`},
		{http.MethodDelete, "/api/schedules/j1", ``},
		{http.MethodPost, "/api/catalog/refresh", ``},
		{http.MethodPost, "/api/sessions/abc", `{"title":"renamed"}`},
		{http.MethodDelete, "/api/sessions/abc", ``},
		{http.MethodPost, "/api/todos", `{"session":"s","content":"t","status":"completed"}`},
		{http.MethodPost, "/api/todos", `{"session":"s","content":"t","status":"bogus"}`},
		{http.MethodPost, "/api/projects", `{"root":"/w/a"}`},
	}
	for _, c := range cases {
		rec := sendJSON(t, server.Handler(), c.method, c.target, initData, c.payload)
		want := http.StatusOK
		if strings.Contains(c.payload, "bogus") {
			want = http.StatusBadRequest
		}
		if rec.Code != want {
			t.Errorf("%s %s = %d, want %d: %s", c.method, c.target, rec.Code, want, rec.Body.String())
		}
	}
}

// A build without the wiring must report itself. Every control route with a
// nil function answers 503, so the page can show "not exposed" instead of an
// empty account that looks like the user owns nothing.
func TestControlWithoutWiringReportsItself(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	initData := initDataFor(t, testToken, testUser, 0)
	for _, target := range []string{
		"/api/meta", "/api/providers", "/api/runparams", "/api/tools",
		"/api/memories", "/api/skills", "/api/schedules", "/api/stats",
		"/api/files", "/api/status", "/api/subscriptions", "/api/catalog",
		"/api/snapshots", "/api/todos",
	} {
		rec := do(t, server.Handler(), http.MethodGet, target, initData)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s = %d, want 503 without wiring", target, rec.Code)
		}
	}
}
