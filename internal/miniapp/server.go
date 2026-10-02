package miniapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ceraa/harness/event"
	"ceraa/harness/sessionstore"
	"ceraa/internal/envs"
	"ceraa/internal/logger"
	"ceraa/internal/paths"
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
}

// Server is the loopbound HTTP surface. One per process, started once and shut
// down in the deferred chain of whatever started it.
type Server struct {
	cfg      Config
	mux      *http.ServeMux
	http     *http.Server
	listener net.Listener
}

// New builds the routes without binding anything, so a test can drive the
// handlers through httptest without a socket.
func New(cfg Config) *Server {
	s := &Server{cfg: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /api/sessions", s.handleSessions)
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
	events := s.cfg.Bus.Subscribe(key, 64)
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

func writeJSON(w http.ResponseWriter, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(payload)
}
