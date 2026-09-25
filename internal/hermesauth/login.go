package hermesauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const deviceCodeGrantType = "urn:ietf:params:oauth:grant-type:device_code"

type LoginFlow struct {
	mu              sync.RWMutex
	ID              string
	Provider        string
	UserCode        string
	VerificationURL string
	Status          string
	Message         string
	ExpiresAt       time.Time
	deviceCode      string
	portalBaseURL   string
	clientID        string
	scope           string
	interval        time.Duration
	cancel          context.CancelFunc
}

type LoginManager struct {
	mu    sync.Mutex
	store *Store
	flows map[string]*LoginFlow
}

func NewLoginManager(store *Store) *LoginManager {
	if store == nil {
		store = NewStore()
	}
	return &LoginManager{store: store, flows: map[string]*LoginFlow{}}
}

func (m *LoginManager) Start(ctx context.Context, providerID string) (*LoginFlow, error) {
	providerID = normalizeProvider(providerID)
	if providerID != "nous" {
		return nil, fmt.Errorf("native login is not implemented for hermes provider %q", providerID)
	}
	portalBaseURL := firstNonEmpty(m.store.PortalURL, defaultNousPortalURL)
	requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	device, err := m.store.requestDeviceCode(requestCtx, portalBaseURL)
	cancel()
	if err != nil {
		return nil, err
	}
	flowCtx, flowCancel := context.WithCancel(context.Background())
	flow := &LoginFlow{
		ID:              fmt.Sprintf("hermes-%d", time.Now().UnixNano()),
		Provider:        providerID,
		UserCode:        device.UserCode,
		VerificationURL: firstNonEmpty(device.VerificationURIComplete, device.VerificationURI),
		Status:          "waiting",
		Message:         "Waiting for browser approval",
		ExpiresAt:       time.Now().Add(time.Duration(device.ExpiresIn) * time.Second),
		deviceCode:      device.DeviceCode,
		portalBaseURL:   portalBaseURL,
		clientID:        defaultNousClientID,
		scope:           "inference:invoke",
		interval:        time.Duration(max(device.Interval, 1)) * time.Second,
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
		return nil, fmt.Errorf("login flow %q not found", flowID)
	}
	return flow, nil
}

func (m *LoginManager) Cancel(flowID string) error {
	flow, err := m.Check(flowID)
	if err != nil {
		return err
	}
	flow.cancel()
	flow.mu.Lock()
	flow.Status = "failed"
	flow.Message = "Login cancelled"
	flow.mu.Unlock()
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

func (m *LoginManager) poll(ctx context.Context, flow *LoginFlow) {
	for {
		if time.Now().After(flow.ExpiresAt) {
			flow.setFailed("Authorization expired")
			return
		}
		payload, pollErr := m.store.pollDeviceToken(ctx, flow.portalBaseURL, flow.clientID, flow.deviceCode)
		if pollErr == nil {
			accessToken, _ := payload["access_token"].(string)
			refreshToken, _ := payload["refresh_token"].(string)
			inferenceURL, _ := payload["inference_base_url"].(string)
			scope, _ := payload["scope"].(string)
			tokenType, _ := payload["token_type"].(string)
			credential := Credential{
				Provider:      "nous",
				AccessToken:   strings.TrimSpace(accessToken),
				RefreshToken:  strings.TrimSpace(refreshToken),
				BaseURL:       firstNonEmpty(inferenceURL, defaultNousInferenceURL),
				PortalBaseURL: flow.portalBaseURL,
				ClientID:      flow.clientID,
				TokenType:     firstNonEmpty(tokenType, "Bearer"),
				ExpiresAt:     expiryFromPayload(payload),
				Source:        "ceraa-native",
			}
			if credential.AccessToken == "" {
				flow.setFailed("Token response did not include access_token")
				return
			}
			if credential.RefreshToken == "" {
				flow.setFailed("Token response did not include refresh_token")
				return
			}
			if credential.ExpiresAt.IsZero() {
				credential.ExpiresAt = nativeCredentialExpiry(credential)
			}
			if scope != "" {
				credential.ClientID = flow.clientID
			}
			if err := m.store.saveNative(credential); err != nil {
				flow.setFailed("Could not save login: " + err.Error())
				return
			}
			flow.mu.Lock()
			flow.Status = "succeeded"
			flow.Message = "Login successful"
			flow.mu.Unlock()
			return
		}
		var pending *devicePollError
		if !errors.As(pollErr, &pending) {
			flow.setFailed(pollErr.Error())
			return
		}
		switch pending.Code {
		case "authorization_pending":
		case "slow_down":
			flow.mu.Lock()
			flow.interval += time.Second
			flow.mu.Unlock()
		default:
			flow.setFailed(pending.Message)
			return
		}
		select {
		case <-ctx.Done():
			flow.setFailed("Login cancelled")
			return
		case <-time.After(flow.interval):
		}
	}
}

func (f *LoginFlow) setFailed(message string) {
	f.mu.Lock()
	f.Status = "failed"
	f.Message = message
	f.mu.Unlock()
}

func (f *LoginFlow) StatusSnapshot() (string, string, string) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.Status, f.Message, f.UserCode
}

func (f *LoginFlow) ResultMessage() string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	switch f.Status {
	case "succeeded":
		return "✅ Hermes/Nous login successful. The Ceraa Hermes provider is ready."
	case "failed":
		return "❌ Hermes login failed: " + f.Message
	default:
		return fmt.Sprintf("🔐 Hermes Device Login\n\n1. Open: %s\n2. Enter code: <code>%s</code>\n3. Approve in the browser, then tap Check Login Status", f.VerificationURL, f.UserCode)
	}
}

func (f *LoginFlow) InlineKeyboardJSON() ([]byte, error) {
	return json.Marshal(map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "🔗 Open Authorization URL", "url": f.VerificationURL}},
		{{"text": "✓ Check Login Status", "callback_data": "hermes:check:" + f.ID}, {"text": "✗ Cancel", "callback_data": "hermes:cancel:" + f.ID}},
	}})
}

type deviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type devicePollError struct {
	Code    string
	Message string
}

func (e *devicePollError) Error() string {
	return e.Message
}

func (s *Store) requestDeviceCode(ctx context.Context, portalBaseURL string) (deviceCodeResponse, error) {
	form := url.Values{"client_id": {defaultNousClientID}, "scope": {"inference:invoke"}}
	payload, err := s.postDeviceJSON(ctx, portalBaseURL+"/api/oauth/device/code", form)
	if err != nil {
		return deviceCodeResponse{}, err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return deviceCodeResponse{}, err
	}
	var device deviceCodeResponse
	if err := json.Unmarshal(encoded, &device); err != nil {
		return deviceCodeResponse{}, err
	}
	if device.DeviceCode == "" || device.UserCode == "" || device.ExpiresIn <= 0 {
		return deviceCodeResponse{}, errors.New("device authorization response is incomplete")
	}
	return device, nil
}

func (s *Store) pollDeviceToken(ctx context.Context, portalBaseURL, clientID, deviceCode string) (map[string]any, error) {
	form := url.Values{"grant_type": {deviceCodeGrantType}, "client_id": {clientID}, "device_code": {deviceCode}}
	return s.postDeviceJSON(ctx, portalBaseURL+"/api/oauth/token", form)
}

func (s *Store) postDeviceJSON(ctx context.Context, endpoint string, form url.Values) (map[string]any, error) {
	if err := validateTokenEndpoint(endpoint); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	client := s.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			return nil, errors.New("authorization endpoint returned invalid JSON")
		}
		return nil, fmt.Errorf("authorization endpoint returned HTTP %d", response.StatusCode)
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return payload, nil
	}
	code, _ := payload["error"].(string)
	description, _ := payload["error_description"].(string)
	if code == "authorization_pending" || code == "slow_down" {
		return nil, &devicePollError{Code: code, Message: code}
	}
	if response.StatusCode >= 500 || response.StatusCode == http.StatusTooManyRequests {
		return nil, &devicePollError{Code: "temporarily_unavailable", Message: fmt.Sprintf("authorization endpoint returned HTTP %d", response.StatusCode)}
	}
	if description == "" {
		description = fmt.Sprintf("authorization failed with HTTP %d", response.StatusCode)
	}
	return nil, &devicePollError{Code: code, Message: description}
}
