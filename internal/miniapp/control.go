package miniapp

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Control surface: the dashboard's read and write API.
//
// Every handler here is thin by design: it parses the request, calls one
// injected Config function, and writes the result. The implementations live
// with the bot that owns the stores, because this package must never import a
// frontend package and a reader that has been admitted still cannot reach a
// tool through here. A nil function is a 503, the same answer Settings gives
// when a surface does not expose it, so a build without the wiring reports
// itself instead of failing in a way that looks like an empty account.

// ProviderEntry is one stored provider profile, without any credential. The
// key behind a profile is never in this response for the same reason secret
// values are never listed: a surface that can show one has a leak it has not
// had yet.
type ProviderEntry struct {
	ID            string `json:"id"`
	Endpoint      string `json:"endpoint"`
	Model         string `json:"model"`
	Reasoning     string `json:"reasoning,omitempty"`
	ContextLimit  int    `json:"context_limit,omitempty"`
	OutputCap     int    `json:"output_cap,omitempty"`
	CacheMode     string `json:"cache_mode,omitempty"`
	PricingUsable bool   `json:"pricing_usable"`
	HasKey        bool   `json:"has_key"`
}

// ToolEntry names one tool the runner can offer the model.
type ToolEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// MemoryEntry is one stored memory with the fields the dashboard shows. No
// score is included: scores decay with time and differ per retrieval, so a
// stored number would be stale on arrival.
type MemoryEntry struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Content   string `json:"content"`
	Veracity  string `json:"veracity"`
	Seen      int    `json:"seen"`
	Applies   string `json:"applies,omitempty"`
	Project   string `json:"project,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

// SubscriptionEntry is one subscription provider's sign-in state. There is
// no account identifier: no cheap source names the account, so the page shows
// signed-in state and which provider is active rather than a name it cannot
// verify.
type SubscriptionEntry struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	SignedIn bool   `json:"signed_in"`
	Active   bool   `json:"active"`
}

// SecretEntry names one stored user secret with its length, never its value.
type SecretEntry struct {
	Name   string `json:"name"`
	Length string `json:"length"`
}

// SkillProposal is one proposed skill awaiting a decision.
type SkillProposal struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Summary string `json:"summary"`
	Source  string `json:"source,omitempty"`
	Status  string `json:"status"`
}

// ScheduleJob is one cron job. The prompt is included because a schedule
// nobody can read back is a schedule nobody can audit.
type ScheduleJob struct {
	ID       string `json:"id"`
	Cron     string `json:"cron"`
	Task     string `json:"task"`
	Session  string `json:"session,omitempty"`
	LastRun  string `json:"last_run,omitempty"`
	LastStat string `json:"last_status,omitempty"`
}

// FileEntry is one directory member for the file browser.
type FileEntry struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"is_dir"`
}

// SnapshotEntry is one file snapshot: what was captured and when. Snapshots
// are file-level, not session-scoped, so the page shows recent ones rather
// than attributing them to a session they were never linked to.
type SnapshotEntry struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Time  string `json:"time"`
}

// TodoEntry is one session todo. Done means completed; anything else is open,
// because the store's statuses beyond completed are degrees of open.
type TodoEntry struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Done bool   `json:"done"`
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Meta == nil {
		http.Error(w, "metadata is not exposed on this surface", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{"meta": s.cfg.Meta()})
}

func (s *Server) handleSubscriptions(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Subscriptions == nil {
		http.Error(w, "subscriptions are not exposed on this surface", http.StatusServiceUnavailable)
		return
	}
	entries, err := s.cfg.Subscriptions()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []SubscriptionEntry{}
	}
	writeJSON(w, map[string]any{"subscriptions": entries})
}

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Catalog == nil {
		http.Error(w, "catalog is not exposed on this surface", http.StatusServiceUnavailable)
		return
	}
	snapshot, err := s.cfg.Catalog()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if snapshot == nil {
		snapshot = map[string]any{}
	}
	writeJSON(w, map[string]any{"catalog": snapshot})
}

func (s *Server) handleRefreshCatalog(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RefreshCatalog == nil {
		http.Error(w, "catalog cannot be refreshed from this surface", http.StatusServiceUnavailable)
		return
	}
	snapshot, err := s.cfg.RefreshCatalog()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if snapshot == nil {
		snapshot = map[string]any{}
	}
	writeJSON(w, map[string]any{"catalog": snapshot})
}

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Providers == nil {
		http.Error(w, "providers are not exposed on this surface", http.StatusServiceUnavailable)
		return
	}
	active, list, err := s.cfg.Providers()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []ProviderEntry{}
	}
	writeJSON(w, map[string]any{"active": active, "providers": list})
}

func (s *Server) handleSetActiveProvider(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SetActiveProvider == nil {
		http.Error(w, "providers cannot be changed on this surface", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Active string `json:"active"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, "expected a JSON object", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Active) == "" {
		http.Error(w, "no provider named", http.StatusBadRequest)
		return
	}
	if err := s.cfg.SetActiveProvider(strings.TrimSpace(body.Active)); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.handleProviders(w, r)
}

func (s *Server) handleSaveProvider(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SaveProvider == nil {
		http.Error(w, "providers cannot be changed on this surface", http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "no provider named", http.StatusBadRequest)
		return
	}
	var profile map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&profile); err != nil {
		http.Error(w, "expected a JSON object", http.StatusBadRequest)
		return
	}
	if err := s.cfg.SaveProvider(id, profile); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.handleProviders(w, r)
}

func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	if s.cfg.DeleteProvider == nil {
		http.Error(w, "providers cannot be changed on this surface", http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "no provider named", http.StatusBadRequest)
		return
	}
	if err := s.cfg.DeleteProvider(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.handleProviders(w, r)
}

func (s *Server) handleRunParams(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunParams == nil {
		http.Error(w, "run parameters are not exposed on this surface", http.StatusServiceUnavailable)
		return
	}
	params := s.cfg.RunParams()
	if params == nil {
		params = map[string]any{}
	}
	writeJSON(w, map[string]any{"params": params})
}

func (s *Server) handleSetRunParams(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SetRunParams == nil {
		http.Error(w, "run parameters cannot be changed on this surface", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Params map[string]any `json:"params"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body); err != nil {
		http.Error(w, "expected a JSON object", http.StatusBadRequest)
		return
	}
	if len(body.Params) == 0 {
		http.Error(w, "no parameters to change", http.StatusBadRequest)
		return
	}
	if err := s.cfg.SetRunParams(body.Params); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.handleRunParams(w, r)
}

func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Tools == nil {
		http.Error(w, "tools are not exposed on this surface", http.StatusServiceUnavailable)
		return
	}
	allowed, inventory, err := s.cfg.Tools()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if allowed == nil {
		allowed = []string{}
	}
	if inventory == nil {
		inventory = []ToolEntry{}
	}
	writeJSON(w, map[string]any{"always_allow": allowed, "inventory": inventory})
}

func (s *Server) handleSetToolAllowed(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SetToolAllowed == nil {
		http.Error(w, "tools cannot be changed on this surface", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Name    string `json:"name"`
		Allowed *bool  `json:"allowed"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, "expected a JSON object", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Name) == "" || body.Allowed == nil {
		http.Error(w, "a tool name and an allowed value are required", http.StatusBadRequest)
		return
	}
	if err := s.cfg.SetToolAllowed(strings.TrimSpace(body.Name), *body.Allowed); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.handleTools(w, r)
}

func (s *Server) handleMemories(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Memories == nil {
		http.Error(w, "memories are not exposed on this surface", http.StatusServiceUnavailable)
		return
	}
	entries, err := s.cfg.Memories()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []MemoryEntry{}
	}
	writeJSON(w, map[string]any{"memories": entries})
}

func (s *Server) handleDeleteMemory(w http.ResponseWriter, r *http.Request) {
	if s.cfg.DeleteMemory == nil {
		http.Error(w, "memories cannot be changed on this surface", http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "no memory named", http.StatusBadRequest)
		return
	}
	if err := s.cfg.DeleteMemory(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"id": id, "deleted": true})
}

func (s *Server) handleCompaction(w http.ResponseWriter, r *http.Request) {
	if s.cfg.TriggerCompaction == nil {
		http.Error(w, "compaction cannot be triggered from this surface", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body)
	report, err := s.cfg.TriggerCompaction(strings.TrimSpace(body.SessionID))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if report == nil {
		report = map[string]any{}
	}
	writeJSON(w, map[string]any{"compaction": report})
}

func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Skills == nil {
		http.Error(w, "skills are not exposed on this surface", http.StatusServiceUnavailable)
		return
	}
	proposals, err := s.cfg.Skills()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if proposals == nil {
		proposals = []SkillProposal{}
	}
	writeJSON(w, map[string]any{"proposals": proposals})
}

func (s *Server) handleDecideSkill(w http.ResponseWriter, r *http.Request) {
	if s.cfg.DecideSkill == nil {
		http.Error(w, "skills cannot be decided on this surface", http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	var body struct {
		Decision string `json:"decision"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, "expected a JSON object", http.StatusBadRequest)
		return
	}
	var approve bool
	switch strings.ToLower(strings.TrimSpace(body.Decision)) {
	case "approve":
		approve = true
	case "reject":
		approve = false
	default:
		http.Error(w, "decision must be approve or reject", http.StatusBadRequest)
		return
	}
	if id == "" {
		http.Error(w, "no proposal named", http.StatusBadRequest)
		return
	}
	if err := s.cfg.DecideSkill(id, approve); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.handleSkills(w, r)
}

func (s *Server) handleSchedules(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Schedules == nil {
		http.Error(w, "schedules are not exposed on this surface", http.StatusServiceUnavailable)
		return
	}
	jobs, err := s.cfg.Schedules()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if jobs == nil {
		jobs = []ScheduleJob{}
	}
	writeJSON(w, map[string]any{"jobs": jobs})
}

func (s *Server) handleAddSchedule(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AddSchedule == nil {
		http.Error(w, "schedules cannot be changed on this surface", http.StatusServiceUnavailable)
		return
	}
	var job map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&job); err != nil {
		http.Error(w, "expected a JSON object", http.StatusBadRequest)
		return
	}
	id, err := s.cfg.AddSchedule(job)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"id": id, "added": true})
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	if s.cfg.DeleteSchedule == nil {
		http.Error(w, "schedules cannot be changed on this surface", http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "no job named", http.StatusBadRequest)
		return
	}
	if err := s.cfg.DeleteSchedule(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"id": id, "deleted": true})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Stats == nil {
		http.Error(w, "stats are not exposed on this surface", http.StatusServiceUnavailable)
		return
	}
	stats, err := s.cfg.Stats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if stats == nil {
		stats = map[string]any{}
	}
	writeJSON(w, map[string]any{"stats": stats})
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Files == nil {
		http.Error(w, "files are not exposed on this surface", http.StatusServiceUnavailable)
		return
	}
	entries, err := s.cfg.Files(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if entries == nil {
		entries = []FileEntry{}
	}
	writeJSON(w, map[string]any{"files": entries})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Status == nil {
		http.Error(w, "status is not exposed on this surface", http.StatusServiceUnavailable)
		return
	}
	status, err := s.cfg.Status()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if status == nil {
		status = map[string]any{}
	}
	writeJSON(w, map[string]any{"status": status})
}

func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Snapshots == nil {
		http.Error(w, "snapshots are not exposed on this surface", http.StatusServiceUnavailable)
		return
	}
	entries, err := s.cfg.Snapshots()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []SnapshotEntry{}
	}
	writeJSON(w, map[string]any{"snapshots": entries})
}

func (s *Server) handleTodos(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Todos == nil {
		http.Error(w, "todos are not exposed on this surface", http.StatusServiceUnavailable)
		return
	}
	entries, err := s.cfg.Todos(r.URL.Query().Get("session"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if entries == nil {
		entries = []TodoEntry{}
	}
	writeJSON(w, map[string]any{"todos": entries})
}

func (s *Server) handleRenameSession(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RenameSession == nil {
		http.Error(w, "sessions cannot be renamed on this surface", http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	var body struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, "expected a JSON object", http.StatusBadRequest)
		return
	}
	if id == "" || strings.TrimSpace(body.Title) == "" {
		http.Error(w, "a session id and a title are required", http.StatusBadRequest)
		return
	}
	if err := s.cfg.RenameSession(id, strings.TrimSpace(body.Title)); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"id": id, "renamed": true})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if s.cfg.DeleteSession == nil {
		http.Error(w, "sessions cannot be deleted on this surface", http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "no session named", http.StatusBadRequest)
		return
	}
	if err := s.cfg.DeleteSession(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"id": id, "deleted": true})
}

func (s *Server) handleSetTodo(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SetTodo == nil {
		http.Error(w, "todos cannot be changed on this surface", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Session string `json:"session"`
		Content string `json:"content"`
		Status  string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body); err != nil {
		http.Error(w, "expected a JSON object", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Session) == "" || strings.TrimSpace(body.Content) == "" {
		http.Error(w, "a session and a todo are required", http.StatusBadRequest)
		return
	}
	status := strings.ToLower(strings.TrimSpace(body.Status))
	if status != "completed" && status != "pending" {
		http.Error(w, "status must be completed or pending", http.StatusBadRequest)
		return
	}
	if err := s.cfg.SetTodo(strings.TrimSpace(body.Session), body.Content, status); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"updated": true})
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Projects == nil {
		http.Error(w, "projects are not exposed on this surface", http.StatusServiceUnavailable)
		return
	}
	projects, err := s.cfg.Projects()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if projects == nil {
		projects = map[string]any{}
	}
	writeJSON(w, map[string]any{"projects": projects})
}

func (s *Server) handleSetProject(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SetProject == nil {
		http.Error(w, "projects cannot be changed on this surface", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Root string `json:"root"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, "expected a JSON object", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Root) == "" {
		http.Error(w, "no project root named", http.StatusBadRequest)
		return
	}
	if err := s.cfg.SetProject(strings.TrimSpace(body.Root)); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.handleProjects(w, r)
}

// handleStop cancels the run the status endpoint is currently reporting.
//
// The chat id is taken from the request because the surface serves exactly one
// authorised user, and that user can be in more than one chat. Stopping "the
// user" would pick a run by luck of map order; stopping the chat the button was
// rendered for stops the run the operator is looking at. That is the whole
// point of the control, and the reason this route exists rather than reusing the
// session-scoped cancel, which only ever reaches this surface's own (never
// started) session.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if s.cfg.StopActiveRun == nil {
		http.Error(w, "runs cannot be stopped from this surface", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Chat int64 `json:"chat"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, "expected a JSON object", http.StatusBadRequest)
		return
	}
	if body.Chat == 0 {
		http.Error(w, "no chat named", http.StatusBadRequest)
		return
	}
	stopped, sessionID := s.cfg.StopActiveRun(body.Chat)
	if !stopped {
		// 409 rather than a success. A stop that found nothing has not stopped
		// anything, and returning 200 would let the button report a halt that
		// did not happen.
		http.Error(w, "no active run to cancel", http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"cancelled": true, "session": sessionID})
}
