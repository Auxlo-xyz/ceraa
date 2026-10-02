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
	"strconv"
	"strings"
	"sync"
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

// assetPattern is the mux pattern for the shell. It is named because Handler
// compares the matched pattern against it to decide which requests may arrive
// without a credential, and a bare "/" in that comparison would be a coincidence
// rather than a decision.
const assetPattern = "/"

// apiPrefix is the namespace holding everything that carries data. It is refused
// without a credential even on paths that match no route.
const apiPrefix = "/api/"

// maxSessions bounds the list endpoint. The store has no index, so listing
// means opening every file, and a personal history grows longer than anyone
// scrolls.
const maxSessions = 50

// warn reports a refused request to the operator. It is a variable so a test
// can see what was said without installing a logger first.
var warn = func(message string) { logger.Warn(message) }

// note reports a step that succeeded and was worth knowing about. warn is for
// refusals; fetching a client the operator asked for is neither, and routing
// ordinary work through warn would train an operator to skip the warnings.
var note = func(message string) { logger.Info(message) }

// ErrMissingInitData is the refusal for a request that carries no payload to
// verify. It is separate from a bad signature so the log can tell a browser
// opened directly from a token that has been tampered with.
var ErrMissingInitData = errors.New("no init data")

// Enabled reports whether the Mini App surface was asked for.
//
// On unless it is explicitly turned off, which is the opposite of how this read
// while the surface was new: it opened only for an operator who asked. A working
// surface that has to be configured before it exists is one nobody sets up, so
// the question changed from "who is exposed" to "who is left with a feature that
// silently does nothing" — and for a single-operator bot the second is the worse
// answer.
//
// So the explicit values are a list of ways to say no, and everything else is
// yes. That does mean a misspelled setting lands on open rather than closed,
// which is the trade and is stated here rather than discovered later.
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(envs.Get("MINIAPP"))) {
	case "0", "false", "no", "off", "none", "disabled":
		return false
	default:
		return true
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
	// stop cancels the run in flight, if there is one. It is set before the
	// goroutine starts and cleared when it ends, so a nil here and a false in
	// running always mean the same thing rather than two answers about one run.
	stopMu sync.Mutex
	stop   context.CancelFunc
	// log is the replay buffer behind both event transports. It is built in
	// New rather than on first use, so a handler and a test see the same
	// object whether or not anything has subscribed yet.
	log *eventLog
	// logOnce starts the subscriber exactly once, logMu guards the handle it
	// leaves behind, and logStop ends it on shutdown.
	logOnce sync.Once
	logMu   sync.Mutex
	logStop context.CancelFunc
	// base is what a tunnel handshake hangs off. Closing the surface cancels
	// it, which is how a start still waiting for an announcement gives up at
	// once instead of holding shutdown open for the whole of its allowance.
	base       context.Context
	baseCancel context.CancelFunc
	// tunnel is the public front, when one has been asked for. tunnelMu is
	// held across the whole start rather than only around the field: two chat
	// messages arriving together must produce one child and one origin, not
	// two children and an origin that went stale between them.
	tunnelMu sync.Mutex
	tunnel   *Tunnel
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
	s := &Server{cfg: cfg, log: newEventLog()}
	s.base, s.baseCancel = context.WithCancel(context.Background())
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /api/events", s.handlePoll)
	mux.HandleFunc("GET /api/tunnel", s.handleTunnel)
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
	mux.HandleFunc("POST /api/sessions/{id}/cancel", s.handleCancel)
	mux.HandleFunc(assetPattern, s.handleAsset)
	s.mux = mux
	return s
}

// Handler returns the whole surface with authentication in front of it.
//
// Authentication sits outside the mux rather than inside each route, because the
// failure mode of the other arrangement is a route somebody adds later
// serving data to whoever asked first. Here an unknown path still has to pass
// the same check before it can be a 404.
//
// One route is exempt, and it has to be: the catch-all that serves the shell.
// Telegram delivers initData in the URL fragment, and a browser never sends a
// fragment to the server, so the navigation that loads index.html cannot present
// a credential. The page has to arrive before it can read the one it was opened
// with, or every launch is a bare 401 with nothing on screen to explain it.
//
// The exemption is decided by asking the mux which pattern matched rather than
// by listing paths here. Every route carrying data registers its own pattern, so
// a route added later is behind this check without anyone editing this file, and
// only the shell is open. It serves static files that hold no data, and
// resolveAsset refuses any name that escapes the directory, so opening it costs
// nothing but the shell itself.
//
// The /api/ namespace is excluded even where it matches the catch-all. A path
// under it that matches no route falls through to the asset handler, and letting
// that answer with a 404 is how a probe learns which prefixes exist. It has to
// pass the check first and be refused as unauthorised, which is what it did
// before any of this.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.servesShell(r) {
			s.mux.ServeHTTP(w, r)
			return
		}
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

// servesShell reports whether a request may be answered without a credential.
//
// The comparison goes through the mux rather than a list of paths so that a route
// added later inherits the authentication automatically. Two details of the
// pattern matter: Go writes the root as "GET /{$}" rather than "/", and a path
// under /api/ that matches nothing falls through to the catch-all, where it must
// still be refused.
func (s *Server) servesShell(r *http.Request) bool {
	_, pattern := s.mux.Handler(r)
	// The pattern may carry a method and a {$} anchor, neither of which is part
	// of the path it was registered under.
	if _, after, found := strings.Cut(pattern, " "); found {
		pattern = after
	}
	if strings.TrimSuffix(pattern, "{$}") != assetPattern {
		return false
	}
	return !strings.HasPrefix(r.URL.Path, apiPrefix)
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
	// The buffer is filled from the moment the surface is up rather than from
	// the moment a reader first asks, so a run started from elsewhere in the
	// process is not missing from a page that opens a second later.
	s.ensureLog()
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

// StartTunnel puts this surface on the internet and returns where it landed.
//
// Idempotent on purpose. The origin is assigned by the other end and lives only
// as long as the child does, so starting twice would hand out two addresses for
// one page and leave the first one dead without saying so.
//
// The whole start runs under tunnelMu, so a second caller waits for the first
// rather than racing it. That is also why the handshake does not run inside
// close: the caller above holds nothing while it waits, and closing cancels the
// context the wait is on.
func (s *Server) StartTunnel() (string, error) {
	s.tunnelMu.Lock()
	defer s.tunnelMu.Unlock()
	if s.tunnel != nil {
		return s.tunnel.Origin(), nil
	}
	// Checked before anything else, because a surface that has been closed
	// keeps the fields that made it work: the listener address still reads
	// back and would happily be advertised for a port nobody is accepting on.
	if s.base.Err() != nil {
		return "", errors.New("the Mini App surface has been shut down")
	}
	if s.listener == nil {
		return "", errors.New("the Mini App surface is not listening")
	}
	// Resolved here rather than inside startTunnel, so that the reason a client
	// is missing reaches the caller that has somewhere to put it, and so that a
	// copy fetched for this request is logged by something that knows it was
	// this request that fetched it.
	command, provisioning := resolveTunnelClient()
	if command == "" {
		return "", errors.New(provisioning)
	}
	if provisioning != "" {
		note("Mini App tunnel client: " + provisioning)
	}
	tunnel, err := startTunnel(s.base, command, "http://"+s.Address())
	if err != nil {
		return "", err
	}
	s.tunnel = tunnel
	return tunnel.Origin(), nil
}

// EnsureTunnel returns the public origin, opening a new one when the client that
// was serving it has exited.
//
// StartTunnel alone cannot do this, because it hands back the existing origin
// whenever a tunnel is recorded — including one whose process died minutes ago,
// whose hostname resolves nowhere. That is the difference between a caller that
// recovers and one that republishes a corpse on every tick.
func (s *Server) EnsureTunnel() (string, error) {
	s.tunnelMu.Lock()
	if s.tunnel != nil {
		if !s.tunnel.Gone() {
			origin := s.tunnel.Origin()
			s.tunnelMu.Unlock()
			return origin, nil
		}
		// The client is gone. Closing reclaims its context; done is already
		// closed, so the wait cannot block.
		logger.Info("Mini App tunnel client exited; opening another")
		s.tunnel.Close()
		s.tunnel = nil
	}
	s.tunnelMu.Unlock()
	return s.StartTunnel()
}

// TunnelGone reports whether a tunnel was opened and has since exited.
func (s *Server) TunnelGone() bool {
	s.tunnelMu.Lock()
	defer s.tunnelMu.Unlock()
	return s.tunnel != nil && s.tunnel.Gone()
}

// TunnelAddress is the public origin, or "" when the surface has none. It
// never starts one: reading a value and creating a process are different
// requests, and a handler that does both has a side effect its method name
// does not admit to.
func (s *Server) TunnelAddress() string {
	s.tunnelMu.Lock()
	defer s.tunnelMu.Unlock()
	if s.tunnel == nil {
		return ""
	}
	return s.tunnel.Origin()
}

// handleTunnel reports where this surface is reachable from outside the
// machine, so the page can show the address it is being read through without
// the answer having to come back through chat.
func (s *Server) handleTunnel(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"url": s.TunnelAddress()})
}

// Close shuts the listener down. It runs before whatever owns the process tears
// down the tools, so nothing observes a port that still accepts while the
// process is half gone.
// Close stops a run in flight before the listener goes. The deferred chain
// shuts this down before the tools it started are torn down, and a run left
// going through that would be working on a process nobody is listening to.
func (s *Server) Close() error {
	s.stopMu.Lock()
	cancel := s.stop
	s.stopMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return s.close()
}

// close is the part of Close that touches the listener.
func (s *Server) close() error {
	// Before the mutex below, not after: a handshake still holding it has to
	// give up first, and cancelling its context is what makes it. Taking the
	// lock first would wait out the whole startup allowance on the way down.
	s.baseCancel()
	s.tunnelMu.Lock()
	tunnel := s.tunnel
	s.tunnel = nil
	s.tunnelMu.Unlock()
	if tunnel != nil {
		tunnel.Close()
	}
	s.stopLog()
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

// handleEvents streams the log as server-sent events.
//
// It reads from the buffer rather than subscribing to the bus itself, which
// is what lets a reader resume. The bus drops what nobody is listening to, so
// a reader that reconnects has been listening to nothing for the whole length
// of the gap and would otherwise rejoin part way through an answer.
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
	s.ensureLog()
	cursor, ok := cursorFrom(r, s.log.head())
	if !ok {
		http.Error(w, "bad after", http.StatusBadRequest)
		return
	}

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
		return
	}
	// The hello is what a reader measures the transport by, so it goes out
	// before anything has to happen. On a path that forwards a response as it
	// is produced the hello arrives at once; on one that holds a response
	// until it ends it never arrives at all, and the reader stops waiting.
	if _, err := fmt.Fprintf(w, "event: hello\ndata: {\"seq\":%d}\n\n", cursor); err != nil {
		return
	}
	flusher.Flush()

	// A browser left open across a network hiccup otherwise sits silent for a
	// very long time. The comment costs nothing and the client's own reconnect
	// needs to know the socket is alive.
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	for {
		items, stale, wake := s.log.since(cursor)
		if stale {
			// The buffer no longer reaches the number this reader asked from.
			// Say so, then move the cursor to the head: what follows is still
			// worth delivering, but the reader has to be told the middle is
			// gone so it can re-read the transcript rather than keep the hole.
			cursor = s.log.head()
			if _, err := fmt.Fprintf(w, "event: stale\ndata: {}\n\n"); err != nil {
				return
			}
			flusher.Flush()
			continue
		}
		if len(items) > 0 {
			for _, one := range items {
				payload, err := json.Marshal(one.Event)
				if err != nil {
					continue
				}
				if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n",
					one.Seq, one.Event.Type, payload); err != nil {
					return
				}
				cursor = one.Seq
			}
			flusher.Flush()
			continue
		}
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-wake:
		}
	}
}

// maxPollWait is the longest one poll holds its connection open. Short enough
// that a proxy with an idle timeout of its own does not cut it first, and long
// enough that a quiet session costs one request rather than one a second.
const maxPollWait = 25 * time.Second

// handlePoll answers with what a reader has not seen, and then ends.
//
// It exists because a proxy between the browser and this process can hold a
// response until it completes, which makes a stream that never completes a
// response that never arrives. A request that returns is a response that
// arrives on every path there is, so the reader asks for a batch, waits as
// long as it is willing to, and asks again.
func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Bus == nil {
		http.Error(w, "event stream unavailable", http.StatusServiceUnavailable)
		return
	}
	s.ensureLog()
	cursor, ok := cursorFrom(r, s.log.head())
	if !ok {
		http.Error(w, "bad after", http.StatusBadRequest)
		return
	}
	wait := maxPollWait
	if raw := strings.TrimSpace(r.URL.Query().Get("wait")); raw != "" {
		ms, err := strconv.Atoi(raw)
		if err != nil || ms < 0 {
			http.Error(w, "bad wait", http.StatusBadRequest)
			return
		}
		if asked := time.Duration(ms) * time.Millisecond; asked < wait {
			wait = asked
		}
	}

	items, stale, wake := s.log.since(cursor)
	for len(items) == 0 && !stale && wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-wake:
			if !timer.Stop() {
				<-timer.C
			}
			items, stale, wake = s.log.since(cursor)
		case <-timer.C:
			wait = 0
		case <-r.Context().Done():
			timer.Stop()
			return
		}
	}

	// The cursor in the answer is where to ask from next: the last event
	// handed over when there were any, and unchanged when there were not. A
	// reader that times out on an idle session therefore asks from the same
	// place again rather than sliding forward over events it never saw.
	seq := cursor
	if stale {
		// Nothing is handed over across a gap. Replaying what still fits in
		// the buffer would put the tail of the answer on top of a transcript
		// the reader is about to re-read from disk, and the two would disagree
		// about how much of it has already been said.
		seq = s.log.head()
		items = nil
	}
	events := make([]loggedEvent, 0, len(items))
	for _, one := range items {
		events = append(events, one)
		seq = one.Seq
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(struct {
		Seq    int64         `json:"seq"`
		Stale  bool          `json:"stale,omitempty"`
		Events []loggedEvent `json:"events"`
	}{Seq: seq, Stale: stale, Events: events})
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
	// Detached from the request on purpose, for the reason above: background
	// rather than the request context, which the caller is free to abandon.
	// It is this server's context and not the caller's so that the run can be
	// stopped afterwards by somebody who did not start the request.
	ctx, cancel := context.WithCancel(context.Background())
	s.stopMu.Lock()
	s.stop = cancel
	s.stopMu.Unlock()

	go func() {
		defer func() {
			cancel()
			s.stopMu.Lock()
			s.stop = nil
			s.stopMu.Unlock()
			s.running.Store(false)
		}()
		if err := s.cfg.Prompt(ctx, text); err != nil {
			// A cancelled run is somebody pressing stop, not a failure, and
			// saying so in the stream would turn an intended action into an
			// error the reader has to interpret.
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return
			}
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

// handleCancel stops the run this surface started.
//
// Any other session id is a 404 rather than a 403: this surface has no handle
// on a chat's run and should not appear to. Telling a caller the route exists
// for a conversation it cannot reach would be a small lie that costs somebody
// an afternoon.
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if id := r.PathValue("id"); id != s.cfg.SessionID {
		http.Error(w, "only this surface's own session can be cancelled from here", http.StatusNotFound)
		return
	}
	s.stopMu.Lock()
	cancel := s.stop
	s.stopMu.Unlock()
	if cancel == nil {
		http.Error(w, "no run in progress", http.StatusConflict)
		return
	}
	cancel()
	writeJSON(w, map[string]any{"cancelled": true})
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
