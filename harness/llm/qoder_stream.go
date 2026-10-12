package llm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"ceraa/harness/qodersign"
)

// QoderCredentialSource supplies the identity a Qoder signature is bound to.
// The auth package implements it, which keeps this transport free of any
// dependency on how a credential is stored or refreshed.
type QoderCredentialSource interface {
	QoderUID(ctx context.Context) (string, error)
	QoderToken(ctx context.Context) (string, error)
	QoderMachineID(ctx context.Context) (string, error)
}

// QoderRefresher is an optional capability of a QoderCredentialSource. When a
// request is rejected as unauthorised, a fresh exchange is forced and the
// request is rebuilt once, which recovers a token revoked server-side before
// its recorded expiry.
type QoderRefresher interface {
	RefreshQoderToken(ctx context.Context) (string, error)
}

// sharedQoderSigner is process-wide. It carries no identity itself: AuthFields
// and NewContext bind uid and token per call, so one instance serves every
// account and every request, serialised inside.
var (
	qoderSignerMu sync.Mutex
	qoderSigner   *qodersign.Signer
)

func sharedQoderSigner(ctx context.Context) (*qodersign.Signer, error) {
	qoderSignerMu.Lock()
	defer qoderSignerMu.Unlock()
	if qoderSigner != nil {
		return qoderSigner, nil
	}
	signer, err := qodersign.New(ctx)
	if err != nil {
		return nil, err
	}
	qoderSigner = signer
	return signer, nil
}

// streamQoder is the Qoder subscription transport: the encrypted
// agent_chat_generation endpoint with per-request WASM signing, not the public
// OpenAI-compatible host, which rejects directory keys. A plain bearer token
// on this path answers 403, so there is no unauthenticated fallback.
func (c *StreamingClient) streamQoder(ctx context.Context, req StreamRequest, onDelta func(ProviderResponse)) error {
	if c.cfg.QoderCredentials == nil {
		return fmt.Errorf("qoder: no credential source configured; sign in from the provider menu first")
	}
	info, ok := QoderModelFor(c.cfg.Model)
	if !ok {
		return fmt.Errorf("qoder: unknown model %q; pick one the account serves", c.cfg.Model)
	}
	host := strings.TrimSpace(c.cfg.BaseURL)
	if host == "" {
		host = QoderBaseURL
	}

	rebuilt, waited, refreshed := false, false, false
	for {
		uid, token, machine, err := qoderIdentity(ctx, c.cfg.QoderCredentials)
		if err != nil {
			return err
		}
		prepared, err := qoderPrepare(ctx, host, req, info, uid, token, machine)
		if err != nil {
			return err
		}
		status, raw, streamErr := qoderRoundTrip(ctx, c, prepared, onDelta)
		if streamErr == nil {
			return nil
		}
		// A duplicate request id is spent: rebuild once with fresh UUIDs. The
		// payload is minted per attempt, so a retry already carries new ids;
		// this gate only stops a second rebuild from looping on a persistent
		// 409.
		if status == http.StatusConflict && !rebuilt {
			rebuilt = true
			continue
		}
		if (status == http.StatusUnauthorized || status == http.StatusForbidden) && !waited && qoderQueued(raw) {
			waited = true
			delay := qoderQueueDelay(raw)
			if c.cfg.OnRetry != nil {
				c.cfg.OnRetry(fmt.Errorf("qoder: model queued"), 1, delay)
			}
			if !sleepContext(ctx, delay) {
				return context.Cause(ctx)
			}
			continue
		}
		if (status == http.StatusUnauthorized || status == http.StatusForbidden) && !refreshed {
			if refresher, ok := c.cfg.QoderCredentials.(QoderRefresher); ok {
				if _, err := refresher.RefreshQoderToken(ctx); err == nil {
					refreshed = true
					continue
				}
			}
		}
		if status >= 300 {
			return &ProviderError{Status: status, Body: string(raw), Retryable: shouldRetryStatus(status)}
		}
		return streamErr
	}
}

func qoderIdentity(ctx context.Context, source QoderCredentialSource) (uid, token, machine string, err error) {
	if uid, err = source.QoderUID(ctx); err != nil {
		return "", "", "", err
	}
	if strings.TrimSpace(uid) == "" {
		return "", "", "", fmt.Errorf("qoder: no user id stored; sign in again")
	}
	if token, err = source.QoderToken(ctx); err != nil {
		return "", "", "", err
	}
	if strings.TrimSpace(token) == "" {
		return "", "", "", fmt.Errorf("qoder: no access token stored; sign in again")
	}
	if machine, err = source.QoderMachineID(ctx); err != nil {
		return "", "", "", err
	}
	if strings.TrimSpace(machine) == "" {
		return "", "", "", fmt.Errorf("qoder: no machine id stored; sign in again")
	}
	return uid, token, machine, nil
}

func qoderPrepare(ctx context.Context, host string, req StreamRequest, info QoderModel, uid, token, machine string) (*qodersign.PreparedRequest, error) {
	signer, err := sharedQoderSigner(ctx)
	if err != nil {
		return nil, err
	}
	fields, err := qoderAuthFields(signer, uid, token)
	if err != nil {
		return nil, err
	}
	qoderCtx, err := signer.NewContext(machine, qodersign.ClientVersion, qodersign.UserInfo{
		UID:   uid,
		Token: token,
	}, fields, qodersign.DefaultClientMetadata())
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(qoderPayloadFromRequest(req, info))
	if err != nil {
		return nil, fmt.Errorf("qoder: encode request: %w", err)
	}
	return qoderCtx.PrepareInfer(host, string(payload), info.Key, "system")
}

func qoderAuthFields(signer *qodersign.Signer, uid, token string) (map[string]string, error) {
	eui, key, err := signer.AuthFields(qodersign.UserInfo{UID: uid, Token: token})
	if err != nil {
		return nil, err
	}
	return map[string]string{"encrypt_user_info": eui, "key": key}, nil
}

// qoderRoundTrip sends one signed request and streams the reply. It returns
// the HTTP status with the raw error body for the retry policy to read, plus
// the stream error once the body is accepted.
func qoderRoundTrip(ctx context.Context, c *StreamingClient, prepared *qodersign.PreparedRequest, onDelta func(ProviderResponse)) (int, []byte, error) {
	hreq, err := http.NewRequestWithContext(ctx, "POST", prepared.URL, strings.NewReader(prepared.Body))
	if err != nil {
		return 0, nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	for key, value := range prepared.Headers {
		// The signer owns Authorization: it mints Bearer COSY, and a caller
		// value here would replace it with a plain token the server rejects.
		// Content-Type and Accept stay the transport's for the same reason the
		// OpenAI path reserves them.
		switch http.CanonicalHeaderKey(key) {
		case "Content-Type", "Accept":
			continue
		}
		hreq.Header.Set(key, value)
	}
	resp, err := c.doRequest(hreq)
	if err != nil {
		return 0, nil, fmt.Errorf("qoder: request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if readErr != nil {
			raw = []byte(readErr.Error())
		}
		return resp.StatusCode, raw, fmt.Errorf("qoder: HTTP %d", resp.StatusCode)
	}
	if err := readQoderStream(resp.Body, onDelta, c); err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, nil, nil
}

// qoderQueued reports a queue hold rather than a rejection: the model's
// waiting room, which clears on its own, as opposed to an auth failure, which
// never does. Retrying a rejection as a queue burns the budget; failing a
// queue as a rejection tells the user to re-authenticate for no reason.
func qoderQueued(raw []byte) bool {
	lowered := strings.ToLower(string(raw))
	for _, needle := range []string{"10605", "model_queued", "queued", "queue"} {
		if strings.Contains(lowered, needle) {
			return true
		}
	}
	return false
}

// qoderQueueDelay honours a server-sent wait when one is present, capped so a
// hostile or confused value cannot park the run. Thirty seconds is the
// documented room hold; an absent figure waits that long rather than guessing.
func qoderQueueDelay(raw []byte) time.Duration {
	var body struct {
		Delay       int `json:"delay"`
		RetryAfter  int `json:"retry_after"`
		WaitSeconds int `json:"wait_seconds"`
	}
	if err := json.Unmarshal(raw, &body); err == nil {
		for _, seconds := range []int{body.Delay, body.RetryAfter, body.WaitSeconds} {
			if seconds > 0 && seconds <= 60 {
				return time.Duration(seconds) * time.Second
			}
		}
	}
	return 30 * time.Second
}

func shouldRetryStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// qoderModelError is a model_error frame from inside an otherwise accepted
// stream: the transport worked, the model call did not.
type qoderModelError struct {
	message string
}

func (e *qoderModelError) Error() string { return "qoder: " + e.message }

// readQoderStream decodes the double envelope: outer SSE data lines carry an
// envelope whose real chunk sits at body, as a string or an object. Heartbeats
// (empty, or carrying neither choices nor usage) are dropped; [DONE] passes
// through; a model_error frame fails the stream. The unwrapped chunks are
// standard OpenAI server-sent events, so they ride the shared parser rather
// than a second copy of it.
func readQoderStream(r io.Reader, onDelta func(ProviderResponse), c *StreamingClient) error {
	pr, pw := io.Pipe()
	go func() {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 1024*1024), 8*1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" {
				continue
			}
			if data == "[DONE]" {
				_, _ = io.WriteString(pw, "data: [DONE]\n")
				continue
			}
			inner, skip, fail := unwrapQoderEnvelope([]byte(data))
			if fail != nil {
				_ = pw.CloseWithError(fail)
				return
			}
			if skip {
				continue
			}
			if _, err := io.WriteString(pw, "data: "+inner+"\n"); err != nil {
				return
			}
		}
		_ = pw.CloseWithError(scanner.Err())
	}()
	err := c.readStreamWithCompatibility(pr, onDelta, Compatibility{SupportsFinishReason: false})
	if modelErr, ok := unwrapPipeError(err); ok {
		return &StreamError{Err: modelErr, Retryable: false, Partial: true}
	}
	return err
}

// unwrapPipeError recovers a model_error the envelope loop reported through
// the pipe. The shared parser wraps a pipe failure as a read error, which
// would otherwise misreport a server-side refusal as a transport fault.
func unwrapPipeError(err error) (error, bool) {
	var streamErr *StreamError
	if !errors.As(err, &streamErr) {
		return nil, false
	}
	cause := streamErr.Err
	for cause != nil {
		if _, ok := cause.(*qoderModelError); ok {
			return cause, true
		}
		unwrapper, ok := cause.(interface{ Unwrap() error })
		if !ok {
			return nil, false
		}
		cause = unwrapper.Unwrap()
	}
	return nil, false
}

// unwrapQoderEnvelope extracts the inner event from one outer data line. Skip
// is true for heartbeats; fail carries a model_error frame.
func unwrapQoderEnvelope(data []byte) (inner string, skip bool, fail error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return "", true, nil
	}
	var envelope struct {
		Body           json.RawMessage `json:"body"`
		Type           string          `json:"type"`
		Code           string          `json:"code"`
		Message        string          `json:"message"`
		Error          string          `json:"error"`
		ModelError     string          `json:"model_error"`
		ExecutionError string          `json:"execution_failed"`
		Choices        any             `json:"choices"`
		Usage          any             `json:"usage"`
	}
	if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil {
		// Not JSON at all: heartbeat noise on a stream that otherwise only
		// carries JSON envelopes.
		return "", true, nil
	}
	// Refusals first: an error frame carries no body, so a body check first
	// would pass it downstream as an event and it would vanish into an empty
	// delta instead of failing the stream.
	for _, refusal := range []string{envelope.ModelError, envelope.Error, envelope.ExecutionError} {
		if strings.TrimSpace(refusal) != "" {
			return "", false, &qoderModelError{message: refusal}
		}
	}
	if envelope.Code != "" {
		code := strings.TrimSpace(envelope.Code)
		if code != "0" && code != "200" && code != "success" {
			message := strings.TrimSpace(envelope.Message)
			if message == "" {
				message = "error code " + code
			}
			return "", false, &qoderModelError{message: message}
		}
	}
	if len(bytes.TrimSpace(envelope.Body)) == 0 {
		// A bare OpenAI event riding without an envelope still decodes
		// downstream; anything else bodyless is a heartbeat to drop.
		if envelope.Choices != nil || envelope.Usage != nil {
			return trimmed, false, nil
		}
		return "", true, nil
	}
	body := bytes.TrimSpace(envelope.Body)
	if len(body) == 0 || string(body) == "null" {
		return "", true, nil
	}
	if body[0] == '"' {
		var text string
		if err := json.Unmarshal(body, &text); err != nil {
			return "", true, nil
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return "", true, nil
		}
		return text, false, nil
	}
	// An object body is already the inner event; heartbeat objects carry
	// neither choices nor usage, and the shared parser tolerates the rest.
	var probe struct {
		Choices any     `json:"choices"`
		Usage   any     `json:"usage"`
		Done    bool    `json:"done"`
		Finish  *string `json:"finish_reason"`
		Text    string  `json:"text"`
		Content string  `json:"content"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", true, nil
	}
	if probe.Choices == nil && probe.Usage == nil && !probe.Done && probe.Finish == nil && probe.Text == "" && probe.Content == "" {
		return "", true, nil
	}
	return string(body), false, nil
}

// qoderUUID mints a request id. Fresh per attempt: the server treats a
// repeated request_id as a duplicate and answers 409.
func qoderUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// qoderToolResultContent renders native multimodal content parts.
func qoderToolResultContent(text string, paths []string) []any {
	parts := []any{map[string]any{"type": "text", "text": text}}
	for _, path := range paths {
		attachment, err := LoadMediaAttachment(path)
		if err != nil || attachment == nil {
			continue
		}
		parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + attachment.MimeType + ";base64," + attachment.Base64Data()}})
	}
	if len(parts) == 1 {
		return nil
	}
	return parts
}

// qoderPayloadFromRequest renders a run turn into the plaintext body the
// signer encrypts. System text travels in system[], thinking blocks stay
// home (this endpoint has no use for them and replaying them only bloats the
// prompt), and tool pairing is preserved: a tool result without its assistant
// call leaves the model re-calling the same tool forever.
func qoderPayloadFromRequest(req StreamRequest, info QoderModel) map[string]any {
	var system []string
	var history []qodersign.Message
	for _, item := range req.Messages {
		if item.Role == RoleSystem {
			if strings.TrimSpace(item.Text) != "" {
				system = append(system, item.Text)
			}
			continue
		}
		if item.Kind == "reasoning" || strings.TrimSpace(item.Reason) != "" {
			continue
		}
		switch {
		case item.Kind == "tool_call" && item.CallID != "":
			history = append(history, qodersign.Message{
				Role:    string(RoleAssistant),
				Content: item.Text,
				ToolCalls: []qodersign.ToolCall{{
					ID:   item.CallID,
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: item.Name, Arguments: item.Args},
				}},
			})
		case item.Kind == "tool_calls" && len(item.ToolCalls) > 0:
			calls := make([]qodersign.ToolCall, 0, len(item.ToolCalls))
			for i, call := range item.ToolCalls {
				index := i
				calls = append(calls, qodersign.ToolCall{
					ID:    call.ID,
					Type:  "function",
					Index: &index,
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: call.Name, Arguments: call.Args},
				})
			}
			history = append(history, qodersign.Message{
				Role:      string(RoleAssistant),
				Content:   item.Text,
				ToolCalls: calls,
			})
		case item.Kind == "text" && item.Role == RoleTool && item.CallID != "":
			content := any(item.Output)
			if strings.TrimSpace(item.Output) == "" {
				content = item.Text
			}
			text, _ := content.(string)
			if parts := qoderToolResultContent(text, item.Images); len(parts) > 0 {
				content = parts
			}
			history = append(history, qodersign.Message{
				Role:       string(RoleTool),
				Content:    content,
				ToolCallID: item.CallID,
			})
		case item.Kind == "text":
			// Images ride alongside text on this endpoint, but Ceraa items
			// carry file paths that need loading and resizing at send time.
			// That wiring is a separate task; the text travels, the images
			// wait, and neither is silently repointed.
			history = append(history, qodersign.Message{
				Role:    string(item.Role),
				Content: item.Text,
			})
		}
	}

	userText := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		item := req.Messages[i]
		if item.Role == RoleUser && item.Kind == "text" && strings.TrimSpace(item.Text) != "" {
			userText = item.Text
			break
		}
	}

	var tools []qodersign.Tool
	for _, tool := range req.Tools {
		entry := qodersign.Tool{Type: "function"}
		entry.Function.Name = tool.Name
		entry.Function.Description = tool.Description
		entry.Function.Parameters = tool.Parameters
		tools = append(tools, entry)
	}

	effort := string(req.Reason)
	if effort == string(ReasonOff) {
		effort = "none"
	}
	vl := info.Image
	ask := qodersign.Ask{
		ModelKey:        info.Key,
		UserText:        userText,
		SystemText:      strings.Join(system, "\n\n"),
		IsReasoning:     info.Thinking,
		History:         history,
		MaxTokens:       req.MaxTokens,
		ReasoningEffort: effort,
		IsVl:            &vl,
		ContextWindow:   info.ContextWindow,
		DisplayName:     "",
		Format:          "openai",
		MaxInputTokens:  info.ContextWindow,
		Business:        map[string]any{"type": "agent"},
		Tools:           tools,
	}
	return qodersign.BuildPayload(ask, qoderUUID(), qoderUUID())
}
