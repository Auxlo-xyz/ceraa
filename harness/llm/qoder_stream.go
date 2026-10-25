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

	rebuilt, waited, refreshed, retried := false, false, false, false
	holds := 0
	const maxHolds = 3
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
		if streamErrTyped, ok := streamErr.(*StreamError); ok {
			// A capacity hold arrives as a 200 stream carrying a 403 frame, so it
			// never reaches the HTTP branches above. Waiting is the whole point of
			// the frame: the host names the delay because the model is expected to
			// come back, and failing the run there showed the user the raw JSON.
			if refusal, ok := streamErrTyped.Err.(*qoderModelError); ok && refusal.queue > 0 && holds < maxHolds {
				holds++
				if c.cfg.OnRetry != nil {
					c.cfg.OnRetry(fmt.Errorf("qoder: model at capacity"), holds, refusal.queue)
				}
				if !sleepContext(ctx, refusal.queue) {
					return context.Cause(ctx)
				}
				continue
			}
		}
		// A stream that stopped without a terminal event is replayable only
		// while no answer text reached the caller; readQoderStream reports that
		// in Partial. This transport owns its attempt loop, so the retry lives
		// here rather than in the OpenAI-compatible path.
		if streamErrTyped, ok := streamErr.(*StreamError); ok && streamErrTyped.Retryable && !streamErrTyped.Partial {
			if !retried && c.cfg.MaxRetries > 0 {
				retried = true
				delay := retryDelay(streamErrTyped, 0, c.cfg.RetryBackoff)
				if c.cfg.OnRetry != nil {
					c.cfg.OnRetry(streamErrTyped, 1, delay)
				}
				if !sleepContext(ctx, delay) {
					return context.Cause(ctx)
				}
				continue
			}
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
		Delay             int `json:"delay"`
		RetryAfter        int `json:"retry_after"`
		WaitSeconds       int `json:"wait_seconds"`
		RetryAfterSeconds int `json:"retryAfterSeconds"`
		WaitTime          int `json:"waitTime"`
	}
	if err := json.Unmarshal(raw, &body); err == nil {
		for _, seconds := range []int{body.Delay, body.RetryAfter, body.WaitSeconds, body.RetryAfterSeconds, body.WaitTime} {
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

// qoderModelError is a refusal frame from inside an otherwise accepted stream:
// the transport worked, the model call did not.
//
// queue and pricing are classifications, not extra text. A capacity hold is
// temporary and the frame says how long, so the turn waits it out; a plan
// refusal is permanent for this account and retrying burns the budget. The
// host reports both as an HTTP 200 stream carrying a 403 envelope, so the
// status alone cannot tell them apart.
type qoderModelError struct {
	message string
	queue   time.Duration
	pricing bool
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
	sawContent := false
	wrapped := func(delta ProviderResponse) {
		if delta.Content != "" {
			sawContent = true
		}
		onDelta(delta)
	}
	err := c.readStreamWithCompatibility(pr, wrapped, Compatibility{SupportsFinishReason: false})
	if modelErr, ok := unwrapPipeError(err); ok {
		hold := false
		if refusal, ok := modelErr.(*qoderModelError); ok {
			hold = refusal.queue > 0
		}
		return &StreamError{Err: modelErr, Retryable: hold, Partial: sawContent}
	}
	if streamErr, ok := err.(*StreamError); ok {
		// Partial decides whether the retry loop may resend the request. It has
		// to track visible text only: reasoning and tool-call deltas are
		// regenerated safely, and nothing has acted on them, while text already
		// handed to the caller would appear twice if the turn were replayed.
		//
		// The shared parser sets Partial from every emitted field, and Qoder's
		// models think before they answer, so a stream cut during that thinking
		// was marked partial and refused the retry. One dropped upstream
		// connection then failed a run outright with "provider stream ended
		// without a terminal event" while no answer had been shown at all.
		streamErr.Partial = sawContent
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

// qoderRefusalError classifies a refusal by its payload. The host nests these
// as JSON inside JSON strings (a 403 envelope whose message is a 10605 frame
// whose message is the hold), so the search walks the string layers rather
// than trusting the outer shape.
func qoderRefusalError(message string) *qoderModelError {
	hold, pricing := qoderRefusalDetails(message)
	err := &qoderModelError{queue: hold, pricing: pricing}
	switch {
	case hold > 0:
		err.message = "the model is at capacity; the hold is " + hold.Round(time.Second).String()
	case pricing:
		err.message = "this model is not included in your Qoder plan; see " + qoderPricingURL
	default:
		err.message = strings.TrimSpace(message)
		if err.message == "" {
			err.message = "the host refused the request"
		}
	}
	return err
}

const qoderPricingURL = "https://qoder.com/pricing"

// qoderRefusalDetails reads a capacity hold and a plan refusal out of a
// refusal payload, following quoted JSON up to a few levels deep.
func qoderRefusalDetails(text string) (time.Duration, bool) {
	var hold time.Duration
	pricing := false
	var walk func(string, int)
	walk = func(value string, depth int) {
		if depth > 4 {
			return
		}
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return
		}
		if strings.HasPrefix(trimmed, `"`) {
			var inner string
			if json.Unmarshal([]byte(trimmed), &inner) == nil && strings.TrimSpace(inner) != trimmed {
				walk(inner, depth+1)
			}
			return
		}
		if !strings.HasPrefix(trimmed, "{") {
			return
		}
		var object map[string]json.RawMessage
		if json.Unmarshal([]byte(trimmed), &object) != nil {
			return
		}
		for key, raw := range object {
			switch strings.ToLower(key) {
			case "isqueued":
				var queued bool
				if json.Unmarshal(raw, &queued) == nil && queued && hold == 0 {
					hold = 30 * time.Second
				}
			case "retryafterseconds", "waittime", "wait_seconds", "retry_after", "delay":
				var seconds int
				if json.Unmarshal(raw, &seconds) == nil && seconds > 0 && seconds <= 90 {
					hold = time.Duration(seconds) * time.Second
				}
			case "subcode":
				var code string
				if json.Unmarshal(raw, &code) == nil && strings.EqualFold(code, "RESOURCE_LIMIT") && hold == 0 {
					hold = 30 * time.Second
				}
			case "code":
				var code string
				if json.Unmarshal(raw, &code) == nil && code == "10605" && hold == 0 {
					hold = 30 * time.Second
				}
			case "pricingurl":
				var url string
				if json.Unmarshal(raw, &url) == nil && strings.TrimSpace(url) != "" {
					pricing = true
				}
			case "message", "error", "errormessage":
				var inner string
				if json.Unmarshal(raw, &inner) == nil {
					walk(inner, depth+1)
				}
			}
		}
	}
	walk(text, 0)
	return hold, pricing
}

// qoderStatusOK reports the envelope's own success spelling. The live stream
// says "OK" with 200 beside it; older frames carry "200" or "success".
func qoderStatusOK(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "ok", "200", "success", "0":
		return true
	}
	return false
}

// qoderEnvelopeRefusal picks the most useful text from a refused frame: the
// envelope's message, then its body, then the status itself.
func qoderEnvelopeRefusal(body json.RawMessage, message, code string) string {
	for _, candidate := range []string{message, qoderFailureText(body)} {
		if text := strings.TrimSpace(candidate); text != "" {
			return text
		}
	}
	return "status " + code
}

// qoderFailureText renders a refusal carried in any error-shaped field. The
// raw type is deliberate: the host sends a plain string in some frames and an
// OpenAI-style object in others, and a typed string field fails the whole
// decode on the object form, which turned the refusal into a heartbeat and
// left the caller guessing why the stream stopped.
func qoderFailureText(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	var text string
	if json.Unmarshal(trimmed, &text) == nil {
		text = strings.TrimSpace(text)
		// A quoted body is an inner event, and it can be a refusal of its own.
		if strings.HasPrefix(text, "{") {
			return qoderFailureText(json.RawMessage(text))
		}
		return text
	}
	var object struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Error   json.RawMessage `json:"error"`
		Code    json.RawMessage `json:"code"`
	}
	if json.Unmarshal(trimmed, &object) == nil {
		for _, candidate := range []string{object.Message, object.Type, qoderFailureText(object.Error)} {
			if candidate = strings.TrimSpace(candidate); candidate != "" {
				return candidate
			}
		}
		if code := strings.Trim(strings.TrimSpace(string(object.Code)), `"`); code != "" && code != "null" {
			return "error code " + code
		}
	}
	return ""
}

// unwrapQoderEnvelope extracts the inner event from one outer data line. Skip
// is true for heartbeats; fail carries a model_error frame.
func unwrapQoderEnvelope(data []byte) (inner string, skip bool, fail error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return "", true, nil
	}
	var envelope struct {
		Body            json.RawMessage `json:"body"`
		Type            string          `json:"type"`
		Code            string          `json:"code"`
		Message         string          `json:"message"`
		Error           json.RawMessage `json:"error"`
		ModelError      json.RawMessage `json:"model_error"`
		ExecutionError  json.RawMessage `json:"execution_failed"`
		StatusCode      string          `json:"statusCode"`
		StatusCodeValue int             `json:"statusCodeValue"`
		Choices         any             `json:"choices"`
		Usage           any             `json:"usage"`
	}
	if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil {
		// Not JSON at all: heartbeat noise on a stream that otherwise only
		// carries JSON envelopes.
		return "", true, nil
	}
	// Refusals first: an error frame carries no body, so a body check first
	// would pass it downstream as an event and it would vanish into an empty
	// delta instead of failing the stream.
	for _, refusal := range []json.RawMessage{envelope.ModelError, envelope.Error, envelope.ExecutionError} {
		if message := qoderFailureText(refusal); message != "" {
			return "", false, qoderRefusalError(message)
		}
	}
	// The envelope carries its own status, and a refused frame can arrive with
	// HTTP 200 around it. Ignoring it dropped the refusal as a heartbeat and
	// left the caller with "the stream ended without a terminal event", which
	// names a symptom and hides the cause.
	if code := strings.TrimSpace(envelope.StatusCode); code != "" && !qoderStatusOK(code) {
		return "", false, qoderRefusalError(qoderEnvelopeRefusal(envelope.Body, envelope.Message, code))
	}
	if envelope.StatusCodeValue >= 300 {
		code := fmt.Sprintf("%d", envelope.StatusCodeValue)
		return "", false, qoderRefusalError(qoderEnvelopeRefusal(envelope.Body, envelope.Message, code))
	}
	if envelope.Code != "" {
		code := strings.TrimSpace(envelope.Code)
		if code != "0" && code != "200" && code != "success" {
			message := strings.TrimSpace(envelope.Message)
			if message == "" {
				message = "error code " + code
			}
			return "", false, qoderRefusalError(message)
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
		if item.Kind == "reasoning" {
			// A standalone reasoning carrier has nothing else to send, so it is
			// the only item this endpoint drops. The history's Message has no
			// reasoning field, so a message that merely carries one keeps its
			// text and calls and the reasoning is simply not encoded.
			//
			// Skipping every item with a non-empty Reason dropped real turns:
			// once reasoning replay was wired into the builder, an assistant
			// tool call arrived with its reasoning attached, vanished here, and
			// left its tool result with no call to answer. The host refuses that
			// history with a bare "Error in upstream response" and names nothing,
			// so every tool-using Qoder session failed on its second turn.
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
