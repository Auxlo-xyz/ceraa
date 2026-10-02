package miniapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"ceraa/harness/event"
	"ceraa/harness/sessionstore"
	"ceraa/internal/envs"
	"ceraa/internal/logger"
	"ceraa/internal/paths"
	"ceraa/internal/secret"
)

// initDataMaxAge bounds how long a payload stays usable.
//
// An hour would be the textbook answer and a broken one here: a chat page is
// opened once and then kept alive, so every request after the first hour would
// start failing on data that was perfectly genuine when it arrived. A day still
// bounds the exposure and outlives a session anybody would actually sit through.
const initDataMaxAge = 24 * time.Hour

// listenAddress is the default bind. Loopback only, so the port is reachable
// from this machine and from nothing else.
const listenAddress = "127.0.0.1:8787"

// maxSessions bounds the list endpoint. The store has no index, so listing
// means opening every file, and a personal history grows longer than anyone
// scrolls.
const maxSessions = 50

// warn reports a refused request to the operator. It is a variable so a test
// can see what was said without installing a logger first.
var warn = func(message string) { logger.Warn(message) }

// ErrMissingInitData is the refusal for a request that carries no payload to
// verify. It is separate from a bad signature so the log can tell a browser
// opened directly from a token that has been tampered with.
var ErrMissingInitData = errors.New("no init data")

// Enabled reports whether the Mini App surface was asked for.
//
// Only explicit truthy values turn it on. A typo in a variable that opens a
// port must land on "closed", not on "open with a misspelled setting nobody
// meant", which is what a fallback to true would give.
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(envs.Get("MINIAPP"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// Config is everything the server is handed; it holds no other state.
type Config struct {
	// Token is the bot token initData is signed with.
	Token string
	// AllowID is the one user id allowed in, matched exactly as the bot matches
	// it, so the two surfaces cannot disagree about who is admitted.
	AllowID int64
	// Store is read for sessions. It is never written from here.
	Store *sessionstore.Store
	// Bus carries live events. Nil disables the stream rather than panicking.
	Bus *event.Bus
	// Address overrides the loopback default, for a caller that needs a
	// different port rather than a different posture.
	Address string
	// SessionID is the conversation this surface writes to. It is never one of
	// the Telegram sessions: two frontends fighting over one transcript is the
	// race that motivated a separate surface in the first place.
	SessionID string
	// Prompt runs one turn. Injected by the process that owns the runner, so
	// this package never imports one and a reader that has been admitted still
	// cannot reach a tool through here.
	Prompt func(context.Context, string) error
	// AnswerPermission and AnswerQuestion are the two decisions a run can stop
	// and wait for. Injected for the same reason: the manager that owns a
	// pending request stays where it is, and this surface only relays a choice.
	AnswerPermission func(requestID, decision string) bool
	AnswerQuestion   func(requestID, answer string) bool
	// Settings reads a snapshot of what a run will use, and SetSettings
	// applies a partial update. Injected because that state belongs to the
	// process running the bot; a page-serving package has no business knowing
	// which of those keys exist.
	Settings    func() map[string]any
	SetSettings func(update map[string]any) error
}

// Server is the loopbound HTTP surface. One per process, started once and shut
// down in the deferred chain of whatever started it.
type Server struct {
	cfg      Config
	mux      *http.ServeMux
	http     *http.Server
	listener net.Listener
	// running is the web surface's one-run-at-a-time gate. It guards this
	// session only; a Telegram run holds a different key and does not contend.
	running atomic.Bool
}

// New builds the routes without binding anything, so a test can drive the
// handlers through httptest without a socket.
// WebSessionID is the conversation this surface writes to, and the reason it
// is a constant rather than a lookup: there is one web transcript per process,
// it belongs to the one admitted user, and nothing else in the bot has a claim
// on it.
const WebSessionID = "web"

func New(cfg Config) *Server {
	if strings.TrimSpace(cfg.SessionID) == "" {
		cfg.SessionID = WebSessionID
	}
	s := &Server{cfg: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /api/sessions", s.handleSessions)
	mux.HandleFunc("POST /prompt", s.handlePrompt)
	mux.HandleFunc("POST /api/permission", s.handlePermission)
	mux.HandleFunc("POST /api/answer", s.handleAnswer)
	mux.HandleFunc("GET /api/settings", s.handleSettings)
	mux.HandleFunc("POST /api/settings", s.handleSettings)
	mux.HandleFunc("GET /api/secrets", s.handleSecrets)
	mux.HandleFunc("PUT /api/secrets/{name}", s.handleSecretPut)
	mux.HandleFunc("DELETE /api/secrets/{name}", s.handleSecretDelete)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleSession)
	mux.HandleFunc("/", s.handleAsset)
	s.mux = mux
	return s
}

// Handler returns the whole surface with authentication in front of it.
//
// Authentication sits outside the mux rather than inside each route, because
// the failure mode of the other arrangement is a route somebody adds later
// serving data to whoever asked first. Here an unknown path still has to pass
// the same check before it can be a 404.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := s.identify(r)
		if err != nil {
			if !errors.Is(err, ErrMissingInitData) {
				warn("Mini App request refused: " + err.Error())
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !s.admitted(user) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		s.mux.ServeHTTP(w, r)
	})
}

// identify verifies the payload on this request, from a header or from the
// query string.
//
// The query form exists because the first request is a page navigation, and a
// navigation cannot carry a header the page has not loaded yet. Every later
// call goes through Authorization, which is the form the served page uses.
func (s *Server) identify(r *http.Request) (*User, error) {
	initData := ""
	if header := r.Header.Get("Authorization"); header != "" {
		if parts := strings.SplitN(header, " ", 2); len(parts) == 2 && strings.EqualFold(parts[0], "tma") {
			initData = strings.TrimSpace(parts[1])
		}
	}
	if initData == "" {
		initData = r.URL.Query().Get("initData")
	}
	if initData == "" {
		return nil, ErrMissingInitData
	}
	return Verify(initData, s.cfg.Token, initDataMaxAge, time.Now())
}

// admitted mirrors the bot's own gate on the user id alone. A Mini App has a
// user and no chat, so the chat half of the bot's comparison has nothing to
// match against and is left out rather than guessed at.
func (s *Server) admitted(user *User) bool {
	return s.cfg.AllowID > 0 && user.ID == s.cfg.AllowID
}

// Start binds the loopback socket and serves in the background.
func (s *Server) Start() error {
	address := s.cfg.Address
	if address == "" {
		address = strings.TrimSpace(envs.Get("MINIAPP_ADDR"))
	}
	if address == "" {
		address = listenAddress
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", address, err)
	}
	s.listener = listener
	s.http = &http.Server{Handler: s.Handler()}
	go func() {
		if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			warn("Mini App server stopped: " + err.Error())
		}
	}()
	return nil
}

// Address is where the server is actually listening, which is not the address
// it was asked for when the caller passed port zero.
func (s *Server) Address() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Close shuts the listener down. It runs before whatever owns the process tears
// down the tools, so nothing observes a port that still accepts while the
// process is half gone.
func (s *Server) Close() error {
	if s.http == nil {
		return nil
	}
	return s.http.Close()
}

// handleIndex serves the page.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	s.serveAsset(w, r, "index.html")
}

// handleAsset serves anything else on disk under the asset directory.
func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(filepath.Clean(r.URL.Path), "/")
	if name == "" || name == "." {
		s.serveAsset(w, r, "index.html")
		return
	}
	s.serveAsset(w, r, name)
}

// serveAsset resolves name the way the system prompt is resolved: the data
// directory first, then the working directory, then beside the executable.
//
// Nothing is embedded. The invariant build.sh states is that there is no
// go:embed in the tree, and a UI meant to be edited without rebuilding is the
// worst possible place to break it.
func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request, name string) {
	full, ok := resolveAsset(name)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	http.ServeFile(w, r, full)
}

// resolveAsset finds a shipped file, preferring a copy in the data directory so
// an operator can override what the install provided.
func resolveAsset(name string) (string, bool) {
	if filepath.IsAbs(name) || strings.Contains(name, "..") {
		return "", false
	}
	candidates := []string{
		filepath.Join(paths.DataDir(), "miniapp", name),
		filepath.Join("miniapp", name),
	}
	if exe, err := os.Executable(); err == nil {
		if dir, err := filepath.EvalSymlinks(filepath.Dir(exe)); err == nil {
			candidates = append(candidates, filepath.Join(dir, "miniapp", name))
		}
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, true
		}
	}
	return "", false
}

// handleSessions lists what is on disk, most recently used first.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	ids, err := s.cfg.Store.List()
	if err != nil {
		http.Error(w, "session store unavailable", http.StatusInternalServerError)
		return
	}
	summaries := make([]sessionSummary, 0, len(ids))
	for _, id := range ids {
		sess, err := s.cfg.Store.Load(id)
		if err != nil {
			// A file another writer is mid-way through is not a reason to fail
			// the whole list; it is one entry fewer.
			continue
		}
		summaries = append(summaries, summarise(sess))
	}
	sort.Slice(summaries, func(i, j int) bool {
		if !summaries[i].UpdatedAt.Equal(summaries[j].UpdatedAt) {
			return summaries[i].UpdatedAt.After(summaries[j].UpdatedAt)
		}
		return summaries[i].ID > summaries[j].ID
	})
	if len(summaries) > maxSessions {
		summaries = summaries[:maxSessions]
	}
	writeJSON(w, map[string]any{"sessions": summaries})
}

// handleSession returns one session's transcript beside its context and cost.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.cfg.Store.Exists(id) {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	sess, err := s.cfg.Store.Load(id)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	writeJSON(w, detailOf(sess))
}

type sessionSummary struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Messages  int       `json:"messages"`
	Runs      int       `json:"runs"`
	UpdatedAt time.Time `json:"updated_at"`
}

type contextInfo struct {
	EstimatedTokens int `json:"estimated_tokens"`
	Limit           int `json:"limit"`
	Percent         int `json:"percent"`
	Messages        int `json:"messages"`
}

type costInfo struct {
	TotalUSD float64 `json:"total_usd"`
	Known    bool    `json:"known"`
}

type sessionDetail struct {
	ID       string             `json:"id"`
	Title    string             `json:"title"`
	Messages []sessionMsg       `json:"messages"`
	Context  contextInfo        `json:"context"`
	Cost     costInfo           `json:"cost"`
	Runs     []sessionstore.Run `json:"runs"`
}

// sessionMsg is a transcript entry reduced to what a chat surface renders. The
// stored message also carries usage, compaction and reasoning detail, none of
// which a transcript needs and all of which would be re-sent on every open.
type sessionMsg struct {
	Role       string `json:"role"`
	Content    string `json:"content,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	ToolResult string `json:"tool_result,omitempty"`
	Status     string `json:"status,omitempty"`
}

// summarise is the list view of a session.
func summarise(sess *sessionstore.Session) sessionSummary {
	summary := sessionSummary{
		ID:       sess.ID,
		Title:    sess.DisplayTitle(),
		Messages: len(sess.Messages),
		Runs:     len(sess.Runs),
	}
	if len(sess.Runs) > 0 {
		summary.UpdatedAt = sess.Runs[len(sess.Runs)-1].UpdatedAt
	}
	return summary
}

// detailOf is the open view of a session.
//
// The token estimate is the same character-count heuristic /status prints, kept
// identical on purpose: two surfaces dividing the same text differently is how
// a number stops being believed.
func detailOf(sess *sessionstore.Session) sessionDetail {
	detail := sessionDetail{
		ID:       sess.ID,
		Title:    sess.DisplayTitle(),
		Messages: make([]sessionMsg, 0, len(sess.Messages)),
		Runs:     sess.Runs,
	}
	estimated := 0
	for _, message := range sess.Messages {
		estimated += (len(message.Content) + len(message.ToolResult) + len(message.Reasoning)) / 4
		detail.Messages = append(detail.Messages, sessionMsg{
			Role:       string(message.Role),
			Content:    message.Content,
			ToolName:   message.ToolName,
			ToolResult: message.ToolResult,
			Status:     string(message.ResultStatus),
		})
	}
	limit := 0
	if len(sess.Runs) > 0 {
		last := sess.Runs[len(sess.Runs)-1]
		limit = last.ModelContextWindow
		if last.Cost.Known {
			detail.Cost.Known = true
		}
		for i := range sess.Runs {
			if !sess.Runs[i].Cost.Known {
				continue
			}
			detail.Cost.TotalUSD += sess.Runs[i].Cost.TotalUSD
		}
	}
	percent := 0
	if limit > 0 {
		percent = estimated * 100 / limit
	}
	detail.Context = contextInfo{
		EstimatedTokens: estimated,
		Limit:           limit,
		Percent:         percent,
		Messages:        len(sess.Messages),
	}
	return detail
}

// handleEvents streams the bus as server-sent events.
//
// The stream carries whatever the process publishes. Content and tool activity
// do not reach it yet because the coordinator's callbacks are wired by whichever
// surface started the run, and a read-only observer starts nothing; that is the
// work of the phase that adds a runner.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if s.cfg.Bus == nil {
		http.Error(w, "event stream unavailable", http.StatusServiceUnavailable)
		return
	}
	key := event.Wildcard
	if named := r.URL.Query().Get("session"); named != "" {
		key = named
	}
	// A wide channel rather than the tight one a chat subscriber can afford.
	// The renderer on the other side throttles, this one cannot: assistant text
	// arrives as a delta per token and a full channel drops the excess, which
	// for a reader is missing sentences rather than a stale indicator.
	events := s.cfg.Bus.Subscribe(key, 1024)
	defer s.cfg.Bus.Unsubscribe(key, events)

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
		return
	}
	flusher.Flush()

	// A browser left open across a network hiccup otherwise sits silent for a
	// very long time. The comment costs nothing and the client's own reconnect
	// needs to know the socket is alive.
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case one, open := <-events:
			if !open {
				return
			}
			payload, err := json.Marshal(one)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", one.Type, payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// maxPromptChars bounds one message. The model context window is the real
// limit, but a request that is wrong by a factor of a thousand should be
// refused before it reaches a tokenizer rather than after.
const maxPromptChars = 200_000

// handlePrompt starts one turn in this surface's session and answers
// immediately, because the reply is on the event stream and not here.
//
// A long request would tie the run to the connection: closing the tab would
// cancel work that had already been paid for. 202 says the turn was taken, and
// every subsequent line, tool call and failure is on GET /events.
func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Prompt == nil {
		http.Error(w, "prompting is not enabled on this surface", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPromptChars+1024))
	if err != nil {
		http.Error(w, "unreadable request", http.StatusBadRequest)
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "expected a JSON object", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(req.Text)
	switch {
	case text == "":
		http.Error(w, "text is empty", http.StatusBadRequest)
		return
	case len(text) > maxPromptChars:
		http.Error(w, fmt.Sprintf("text is longer than %d bytes", maxPromptChars), http.StatusRequestEntityTooLarge)
		return
	case !s.running.CompareAndSwap(false, true):
		// The equivalent of Telegram's queue, minus the queue: a second message
		// here would interleave with the first in one transcript, so the honest
		// answer is that this surface is busy.
		http.Error(w, "a run is already in progress", http.StatusConflict)
		return
	}

	sessionID := s.cfg.SessionID
	// Detached from the request on purpose, for the reason above. Background
	// rather than the request context, which the caller is free to abandon.
	go func() {
		defer s.running.Store(false)
		ctx := context.Background()
		if err := s.cfg.Prompt(ctx, text); err != nil {
			warn("Mini App run failed: " + err.Error())
			// The bus here rather than the package default, so a caller that
			// supplied its own bus does not leak into a stranger's stream.
			if s.cfg.Bus != nil {
				s.cfg.Bus.Publish(event.Event{Type: event.TypeError, SessionID: sessionID, Data: map[string]any{
					"message": err.Error(), "stage": "dispatch",
				}})
			}
		}
	}()

	writeJSONAt(w, http.StatusAccepted, map[string]any{"accepted": true, "session": sessionID})
}

func writeJSON(w http.ResponseWriter, body any) {
	writeJSONAt(w, http.StatusOK, body)
}

// decisions are the words a manager will accept. Stated here rather than
// imported so the vocabulary the user of this API can pick from is visible in
// one place, and a typo in a request body is a 400 rather than a request id that
// matches nothing and times out five minutes later.
var decisions = map[string]bool{"allowOnce": true, "allowAlways": true, "deny": true}

// decisionRequest is the shared body of the two POSTs a run can be waiting on.
type decisionRequest struct {
	RequestID string `json:"request_id"`
	// Decision is a permission choice, Answer a question reply. They share a
	// struct because they share a shape and a set of failure modes, not because
	// a decision and an answer mean the same thing.
	Decision string `json:"decision"`
	Answer   string `json:"answer"`
}

func readDecision(w http.ResponseWriter, r *http.Request) (decisionRequest, bool) {
	var req decisionRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "expected a JSON object", http.StatusBadRequest)
		return decisionRequest{}, false
	}
	if strings.TrimSpace(req.RequestID) == "" {
		http.Error(w, "request_id is empty", http.StatusBadRequest)
		return decisionRequest{}, false
	}
	return req, true
}

// handlePermission relays a tool decision. The run is already waiting; this is
// only the answer to a question it asked some seconds ago.
func (s *Server) handlePermission(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AnswerPermission == nil {
		http.Error(w, "permissions are not answerable on this surface", http.StatusServiceUnavailable)
		return
	}
	req, ok := readDecision(w, r)
	if !ok {
		return
	}
	if !decisions[req.Decision] {
		http.Error(w, "decision must be allowOnce, allowAlways or deny", http.StatusBadRequest)
		return
	}
	// The manager owns the pending table and answers false for a request that
	// is already gone: expired, answered from the chat, or never existed. A 409
	// says so without inventing a second answer.
	if !s.cfg.AnswerPermission(req.RequestID, req.Decision) {
		http.Error(w, "no pending request with that id", http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"accepted": true})
}

// handleAnswer relays a question reply. The text is passed through untouched:
// this surface does not decide what a good answer is, it decides who may give
// one.
func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AnswerQuestion == nil {
		http.Error(w, "questions are not answerable on this surface", http.StatusServiceUnavailable)
		return
	}
	req, ok := readDecision(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Answer) == "" {
		http.Error(w, "answer is empty", http.StatusBadRequest)
		return
	}
	if !s.cfg.AnswerQuestion(req.RequestID, req.Answer) {
		http.Error(w, "no pending question with that id", http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"accepted": true})
}

// handleSettings reads and writes what a run will use.
//
// One handler for both verbs: the read and the write answer the same question
// from opposite ends, and a write that does not know what a good read looks
// like drifts. A body that changes nothing is rejected rather than accepted as
// a no-op, because a save that silently did nothing is worse than an error.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Settings == nil {
		http.Error(w, "settings are not exposed on this surface", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodGet {
		snapshot := s.cfg.Settings()
		if snapshot == nil {
			snapshot = map[string]any{}
		}
		writeJSON(w, map[string]any{"settings": snapshot})
		return
	}
	if s.cfg.SetSettings == nil {
		http.Error(w, "settings cannot be changed on this surface", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Settings map[string]any `json:"settings"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(w, "expected a JSON object", http.StatusBadRequest)
		return
	}
	if len(body.Settings) == 0 {
		http.Error(w, "no settings to change", http.StatusBadRequest)
		return
	}
	if err := s.cfg.SetSettings(body.Settings); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"settings": s.cfg.Settings()})
}

// maxSecretName and maxSecretValue bound an entry before it is encrypted. The
// store has no limits of its own, and a surface that can write should not be
// able to hand it a megabyte under a name nobody can read in a menu.
const (
	maxSecretName  = 64
	maxSecretValue = 8 << 10
)

func (s *Server) secretName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := strings.TrimSpace(r.PathValue("name"))
	switch {
	case name == "":
		http.Error(w, "secret name is empty", http.StatusBadRequest)
		return "", false
	case len(name) > maxSecretName:
		http.Error(w, fmt.Sprintf("secret name is longer than %d bytes", maxSecretName), http.StatusBadRequest)
		return "", false
	case strings.ContainsAny(name, "\n\r\t"):
		http.Error(w, "secret name contains whitespace", http.StatusBadRequest)
		return "", false
	case secret.IsInternal(name):
		// The same reserve the model's tools face. A name this process uses for
		// itself is not a credential the person at this page owns, and letting
		// it be overwritten from here would be how a bot token gets replaced by
		// something an attacker chose.
		http.Error(w, "that name is reserved by Ceraa", http.StatusForbidden)
		return "", false
	}
	return name, true
}

// handleSecrets lists names and never values.
func (s *Server) handleSecrets(w http.ResponseWriter, r *http.Request) {
	// Names only. The values are not in this response because there is no code
	// path in this server that reads one: a surface which can show a credential
	// has a leak it has not had yet.
	writeJSON(w, map[string]any{"secrets": secret.ListUser()})
}

func (s *Server) handleSecretPut(w http.ResponseWriter, r *http.Request) {
	name, ok := s.secretName(w, r)
	if !ok {
		return
	}
	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxSecretValue+1024)).Decode(&body); err != nil {
		http.Error(w, "expected a JSON object", http.StatusBadRequest)
		return
	}
	if body.Value == "" {
		http.Error(w, "value is empty", http.StatusBadRequest)
		return
	}
	if len(body.Value) > maxSecretValue {
		http.Error(w, fmt.Sprintf("value is longer than %d bytes", maxSecretValue), http.StatusRequestEntityTooLarge)
		return
	}
	if err := secret.Set(name, body.Value); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The value is not echoed back, and it is not in the list either. This
	// surface can write a credential and show that one exists; it cannot read
	// one, so a leaked response body is not a leaked key.
	writeJSON(w, map[string]any{"name": name, "stored": true})
}

func (s *Server) handleSecretDelete(w http.ResponseWriter, r *http.Request) {
	name, ok := s.secretName(w, r)
	if !ok {
		return
	}
	found := false
	for _, existing := range secret.ListUser() {
		if existing == name {
			found = true
			break
		}
	}
	if !found {
		http.Error(w, "no such secret", http.StatusNotFound)
		return
	}
	if err := secret.Delete(name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"name": name, "deleted": true})
}

func writeJSONAt(w http.ResponseWriter, status int, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}
