// Package codexauth implements OAuth device login flow for Codex CLI,
// presenting the authorization URL and user code via Telegram inline keyboards.
package codexauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"ceraa/internal/secret"
)

const (
	deviceURL      = "https://chatgpt.com/codex/device-auth"
	startupTimeout = 30 * time.Second
	authLifetime   = 15 * time.Minute
	pollInterval   = 5 * time.Second
)

var (
	userCodeRe = regexp.MustCompile(`[A-Z0-9]{4,8}\-[A-Z0-9]{4,8}`)
	authURLRe  = regexp.MustCompile(`https://chatgpt\.com/codex/device-auth`)
)

// AuthState tracks an in-progress device login.
type AuthState struct {
	mu        sync.Mutex
	flowID    string
	UserCode  string
	AuthURL   string
	Status    string // "waiting", "checking", "succeeded", "failed"
	Message   string
	ExpiresAt time.Time
	cmd       *exec.Cmd
	done      chan struct{}
}

// Manager coordinates auth flows with Telegram callbacks.
type Manager struct {
	mu     sync.Mutex
	flows  map[string]*AuthState
	cancel map[string]context.CancelFunc
}

// NewManager creates a new auth manager.
func NewManager() *Manager {
	return &Manager{
		flows:  make(map[string]*AuthState),
		cancel: make(map[string]context.CancelFunc),
	}
}

// Start initiates a Codex device login flow.
func (m *Manager) Start() (*AuthState, error) {
	home := os.Getenv("HOME")
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = home + "/.codex"
	}

	ctx, cancel := context.WithTimeout(context.Background(), startupTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "codex", "login", "--device-auth")
	cmd.Env = append(os.Environ(),
		"CODEX_HOME="+codexHome,
		"HOME="+home,
	)

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start codex login: %w", err)
	}

	// Parse output in a goroutine
	go func() {
		// Give it a moment to produce output
		time.Sleep(2 * time.Second)
	}()

	if err := cmd.Wait(); err != nil {
		// Even if exit code is non-zero, output may contain the code
	}

	output := stdout.String() + stderr.String()
	cleanOutput := stripVTControlCharacters(output)

	state := &AuthState{
		flowID:    fmt.Sprintf("codex-%d", time.Now().UnixNano()),
		done:      make(chan struct{}),
		ExpiresAt: time.Now().Add(authLifetime),
	}

	// Parse auth URL and user code
	lines := strings.Split(cleanOutput, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if userCodeRe.MatchString(line) {
			state.UserCode = line
		}
		if authURLRe.MatchString(line) {
			state.AuthURL = line
		}
	}

	if state.UserCode == "" {
		// Fallback: generate a code from output
		code := userCodeRe.FindString(cleanOutput)
		if code != "" {
			state.UserCode = code
		}
	}

	if state.AuthURL == "" {
		state.AuthURL = deviceURL
	}

	state.Status = "waiting"
	state.Message = "Enter code on ChatGPT device auth page"

	m.mu.Lock()
	m.flows[state.flowID] = state
	m.cancel[state.flowID] = cancel
	m.mu.Unlock()

	// Start polling in background
	go m.pollLogin(state)

	return state, nil
}

func (m *Manager) pollLogin(state *AuthState) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			state.mu.Lock()
			if time.Now().After(state.ExpiresAt) {
				state.Status = "failed"
				state.Message = "Authorization expired"
				state.mu.Unlock()
				return
			}
			state.mu.Unlock()

			// Check login status
			home := os.Getenv("HOME")
			codexHome := os.Getenv("CODEX_HOME")
			if codexHome == "" {
				codexHome = home + "/.codex"
			}

			cmd := exec.Command("codex", "login", "status")
			cmd.Env = append(os.Environ(),
				"CODEX_HOME="+codexHome,
				"HOME="+home,
			)
			output, _ := cmd.CombinedOutput()

			state.mu.Lock()
			if strings.Contains(string(output), "logged in") && strings.Contains(string(output), "chatgpt") {
				state.Status = "succeeded"
				state.Message = "Login successful"
				close(state.done)
				// Store credential marker
				_ = secret.Set("codex_authenticated", "true")
				state.mu.Unlock()
				return
			} else if strings.Contains(string(output), "not logged in") {
				state.Status = "waiting"
				state.Message = "Waiting for authorization..."
			}
			state.mu.Unlock()

		case <-state.done:
			return
		}
	}
}

// ListFlows returns all active auth flows.
func (m *Manager) ListFlows() []*AuthState {
	m.mu.Lock()
	defer m.mu.Unlock()
	flows := make([]*AuthState, 0, len(m.flows))
	for _, f := range m.flows {
		flows = append(flows, f)
	}
	return flows
}

// Check returns the current auth status.
func (m *Manager) Check(flowID string) (*AuthState, error) {
	m.mu.Lock()
	state, ok := m.flows[flowID]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("flow %q not found", flowID)
	}
	return state, nil
}

// Cancel terminates an in-progress login.
func (m *Manager) Cancel(flowID string) error {
	m.mu.Lock()
	state, ok := m.flows[flowID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("flow %q not found", flowID)
	}
	if state.cmd != nil {
		_ = state.cmd.Process.Kill()
	}
	close(state.done)
	return nil
}

func stripVTControlCharacters(s string) string {
	var buf bytes.Buffer
	for _, r := range s {
		if r < 32 && r != '\n' && r != '\t' && r != '\r' {
			continue
		}
		buf.WriteRune(r)
	}
	return buf.String()
}

// InlineKeyboardJSON returns the Telegram inline keyboard for auth flow.
func (s *AuthState) InlineKeyboardJSON() ([]byte, error) {
	keyboard := map[string]any{
		"inline_keyboard": [][]map[string]any{
			{
				{
					"text": "🔗 Open Authorization URL",
					"url":  s.AuthURL,
				},
			},
			{
				{
					"text":          "✓ Check Login Status",
					"callback_data": "codex:check:" + s.flowID,
				},
				{
					"text":          "✗ Cancel",
					"callback_data": "codex:cancel:" + s.flowID,
				},
			},
		},
	}
	return json.Marshal(keyboard)
}

// ResultMessage returns the current auth status as a Telegram message.
func (s *AuthState) ResultMessage() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch s.Status {
	case "succeeded":
		return fmt.Sprintf("✅ Codex login successful!\nUser code: %s\n\nYou can now use Codex as an agent.", s.UserCode)
	case "failed":
		return fmt.Sprintf("❌ Login failed: %s\n\nTry again.", s.Message)
	case "waiting":
		return fmt.Sprintf("🔐 Codex Device Login\n\n1. Open: %s\n2. Enter code: <code>%s</code>\n3. Tap 'Check Login Status' after authorizing", s.AuthURL, s.UserCode)
	default:
		return fmt.Sprintf("Status: %s", s.Status)
	}
}
