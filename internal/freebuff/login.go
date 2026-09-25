package freebuff

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// LoginFlow is one in-progress Freebuff device login. FingerprintHash and
// ExpiresAt are stored as opaque strings and echoed back byte-for-byte:
// expiresAt is an HMAC input the status endpoint verifies, so it must never be
// parsed, reformatted, or re-serialized.
type LoginFlow struct {
	mu              sync.RWMutex
	ID              string
	LoginURL        string
	FingerprintID   string
	FingerprintHash string
	expiresAt       string
	Status          string
	Message         string
	attempts        int
	client          *Client
	cancel          context.CancelFunc
}

type LoginManager struct {
	mu     sync.Mutex
	client *Client
	flows  map[string]*LoginFlow
}

func NewLoginManager(client *Client) *LoginManager {
	if client == nil {
		client = NewClient()
	}
	return &LoginManager{client: client, flows: map[string]*LoginFlow{}}
}

// Start requests a login URL and begins polling in the background. The
// fingerprint ID is taken from an existing credential when one is present (so
// an imported Freebuff CLI login keeps using the same device), otherwise a new
// stable one is minted.
func (m *LoginManager) Start(ctx context.Context) (*LoginFlow, error) {
	fingerprintID := newFingerprintID()
	if existing, err := loadUser(); err == nil && existing != nil && existing.FingerprintID != "" {
		fingerprintID = existing.FingerprintID
	}
	reqCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	code, err := m.client.Code(reqCtx, fingerprintID)
	cancel()
	if err != nil {
		return nil, err
	}
	flowCtx, flowCancel := context.WithCancel(context.Background())
	flow := &LoginFlow{
		ID:              fmt.Sprintf("freebuff-%d", time.Now().UnixNano()),
		LoginURL:        code.LoginURL,
		FingerprintID:   fingerprintID,
		FingerprintHash: code.FingerprintHash,
		expiresAt:       code.expiresAtParam(),
		Status:          "waiting",
		Message:         "Waiting for browser approval",
		client:          m.client,
		cancel:          flowCancel,
	}
	m.mu.Lock()
	m.flows[flow.ID] = flow
	m.mu.Unlock()
	go m.poll(flowCtx, flow)
	return flow, nil
}

func (m *LoginManager) Check(flowID string) (*LoginFlow, error) {
	m.mu.Lock()
	flow, ok := m.flows[flowID]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("freebuff login flow %q not found", flowID)
	}
	return flow, nil
}

func (m *LoginManager) Cancel(flowID string) error {
	flow, err := m.Check(flowID)
	if err != nil {
		return err
	}
	if flow.cancel != nil {
		flow.cancel()
	}
	flow.setStatus("failed", "Login cancelled")
	return nil
}

func (m *LoginManager) ListFlows() []*LoginFlow {
	m.mu.Lock()
	defer m.mu.Unlock()
	flows := make([]*LoginFlow, 0, len(m.flows))
	for _, flow := range m.flows {
		flows = append(flows, flow)
	}
	return flows
}

// poll mirrors the Freebuff CLI's login polling loop: only the presence of a
// user object ends polling, and every other outcome is retried. A network
// error is logged-and-continued rather than fatal, matching the CLI.
func (m *LoginManager) poll(ctx context.Context, flow *LoginFlow) {
	start := m.client.now()
	for {
		if ctx.Err() != nil {
			flow.setStatus("failed", "Login cancelled")
			return
		}
		// The CLI's poll timeout is 5 minutes even though the server-side code
		// is valid for an hour.
		if m.client.now().Sub(start) >= PollTimeout {
			flow.setStatus("failed", "Login timed out after 5 minutes. Run /freebuff-login to try again.")
			return
		}
		flow.mu.Lock()
		flow.attempts++
		flow.mu.Unlock()

		user, err := flow.client.Status(ctx, flow.FingerprintID, flow.FingerprintHash, flow.expiresAt)
		if err == nil && user != nil {
			// Persist the token before reporting success so a restart after
			// approval still finds the credential.
			user.FingerprintID = flow.FingerprintID
			if user.FingerprintHash == "" {
				user.FingerprintHash = flow.FingerprintHash
			}
			if saveErr := saveUser(*user); saveErr != nil {
				flow.setStatus("failed", "Could not save login: "+saveErr.Error())
				return
			}
			flow.setStatus("succeeded", "Login successful")
			return
		}
		// Not approved yet (or a transient network error): sleep and retry.
		if serr := m.client.sleep(ctx, PollInterval); serr != nil {
			flow.setStatus("failed", "Login cancelled")
			return
		}
	}
}

func (f *LoginFlow) setStatus(status, message string) {
	f.mu.Lock()
	f.Status = status
	f.Message = message
	f.mu.Unlock()
}

func (f *LoginFlow) StatusSnapshot() (string, string) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.Status, f.Message
}

func (f *LoginFlow) ResultMessage() string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	switch f.Status {
	case "succeeded":
		return "✅ Freebuff login successful. The Sooraa Freebuff provider is ready."
	case "failed":
		return "❌ Freebuff login failed: " + f.Message
	default:
		return fmt.Sprintf("🔐 Freebuff Login\n\n1. Open: %s\n2. Approve the login in your browser\n3. Tap Check Login Status", f.LoginURL)
	}
}

func (f *LoginFlow) InlineKeyboardJSON() ([]byte, error) {
	return json.Marshal(map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "🔗 Open Freebuff sign-in", "url": f.LoginURL}},
		{{"text": "✓ Check Login Status", "callback_data": "freebuff:check:" + f.ID}, {"text": "✗ Cancel", "callback_data": "freebuff:cancel:" + f.ID}},
	}})
}
