package tool

// recon_auth.go is the session jar wearing a tool face.
//
// The gap it closes: the recon layer could probe a URL but could not log in
// and keep the result, so every authenticated test - IDOR, BOLA, an admin
// panel, CSRF - was done by hand. A login here is one request through the
// guarded transport with a cookie jar attached, and what the response set
// becomes a named session on disk that later requests replay.
//
// Three tools, and the split is the workflow: log in once, replay as often
// as the test needs, and see what is stored.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"ceraa/harness/jar"
	"ceraa/harness/netguard"
	"ceraa/harness/operation"
	"ceraa/harness/recon"
	"ceraa/internal/build"
	"ceraa/internal/paths"
)

const (
	// authDefaultTimeout and authMaxTimeout mirror the probe's own bounds.
	authDefaultTimeout = 15
	authMaxTimeout     = 120
	// maxAuthBodyBytes bounds what a login may send and what it may read
	// back. The response body is dropped rather than printed: a login
	// response carries the session, and echoing it puts a credential in
	// the chat.
	maxAuthBodyBytes = 64 * 1024
	// maxAuthHeadRunes bounds what a replay echoes of the response body.
	// The probe already bounds its own head; this is the second bound.
	maxAuthHeadRunes = 1200
)

// authSessionDir holds the named logins. It sits under the recon tree
// because a session is recon state, next to the receipts that recorded how
// it was obtained.
var authSessionDir = filepath.Join(paths.DataDir(), "recon", "sessions")

// authUserAgent identifies the login to the target, the way the probe
// identifies itself: traffic that reaches someone else's logs should be
// findable in them.
var authUserAgent = "ceraa-recon/" + build.Version + " (+https://github.com/emperormk01/ceraa)"

// replayHeaders are the request headers worth keeping with a session.
//
// An Authorization header is a credential the target issued once and will not
// issue again, and the replay needs it and cannot get it any other way. The
// list is short on purpose: everything kept here is written to disk and sent
// again without being asked.
var replayHeaders = []string{
	"authorization",
	"x-api-key",
	"x-auth-token",
	"x-csrf-token",
	"x-xsrf-token",
}

// ---------------------------------------------------------------------------
// auth_login
// ---------------------------------------------------------------------------

// AuthLoginTool performs a login and stores the session it produced.
type AuthLoginTool struct{}

func (a *AuthLoginTool) Name() string { return "auth_login" }

func (a *AuthLoginTool) Description() string {
	return "Log in to a target and keep the session, so later requests can be authenticated without logging in again. Sends one request (POST by default) with the body and headers you give, captures every cookie the response sets, and stores them under a name you choose. Returns the session name, the HTTP status, the names of the cookies captured and the path of the session file. The response body is read but never printed, and cookie values never leave this machine: only their names are reported. The request is scope-gated and metered against the engagement rate budget, exactly like a probe."
}

func (a *AuthLoginTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":    map[string]any{"type": "string", "description": "Name to store the session under, e.g. \"admin\". Letters, digits, dash, underscore and dot only."},
			"url":     map[string]any{"type": "string", "description": "Absolute http:// or https:// URL of the login endpoint"},
			"method":  map[string]any{"type": "string", "description": "HTTP method for the login request, default POST. One of GET, POST, HEAD, OPTIONS, PUT, PATCH, DELETE."},
			"body":    map[string]any{"additionalProperties": true, "description": "Login body: either a string, or a JSON object/array which is serialized and sent as application/json. Capped at 64k"},
			"headers": map[string]any{"type": "object", "additionalProperties": true, "description": "Request headers as name/value pairs; values may be strings, numbers or booleans"},
			"timeout": map[string]any{"type": "integer", "description": "Request timeout in seconds, default 15, maximum 120"},
		},
		"required": []string{"name", "url"},
	}
}

func (a *AuthLoginTool) Translate(args map[string]any) (operation.Spec, error) {
	name, err := authNameArg(args["name"])
	if err != nil {
		return operation.Spec{}, err
	}
	rawURL, err := authURLArg(args["url"])
	if err != nil {
		return operation.Spec{}, err
	}
	method, err := authMethodArg(args["method"], http.MethodPost)
	if err != nil {
		return operation.Spec{}, err
	}
	body, bodyIsJSON, err := recon.NormalizeBody(args["body"])
	if err != nil {
		return operation.Spec{}, err
	}
	headers, err := authHeaders(args["headers"])
	if err != nil {
		return operation.Spec{}, err
	}
	if bodyIsJSON && body != "" && !authHasHeader(headers, "Content-Type") {
		if headers == nil {
			headers = make(map[string]string, 1)
		}
		headers["Content-Type"] = "application/json"
	}
	out := map[string]any{
		"name":    name,
		"url":     rawURL,
		"method":  method,
		"timeout": authTimeoutArg(args["timeout"]),
	}
	if len(headers) > 0 {
		out["headers"] = headers
	}
	if body != "" {
		out["body"] = body
	}
	return operation.Spec{Type: "auth_login", Args: out}, nil
}

func (a *AuthLoginTool) TranslateResult(op operation.Operation) string { return op.Output }

func (a *AuthLoginTool) Run(spec operation.Spec) (string, error) {
	return a.RunContext(context.Background(), spec)
}

func (a *AuthLoginTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	name, err := authNameArg(spec.Args["name"])
	if err != nil {
		return "", err
	}
	rawURL, err := authURLArg(spec.Args["url"])
	if err != nil {
		return "", err
	}
	method, err := authMethodArg(spec.Args["method"], http.MethodPost)
	if err != nil {
		return "", err
	}
	body, err := authBodyArg(spec.Args["body"])
	if err != nil {
		return "", err
	}
	headers, err := authHeaders(spec.Args["headers"])
	if err != nil {
		return "", err
	}
	timeout := authTimeoutArg(spec.Args["timeout"])
	if len(body) > maxAuthBodyBytes {
		return "", fmt.Errorf("body is %dB, over the %dB auth cap", len(body), maxAuthBodyBytes)
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("url is not valid: %w", err)
	}
	// The gate and the budget are charged before the request exists, so a
	// refused login never leaves the machine. Both are the same checks a
	// probe makes: the scope decides whether the destination may be
	// contacted at all, and the budget meters what was sent.
	if err := recon.Gate(rawURL); err != nil {
		return "", fmt.Errorf("refused, not sent: %w", err)
	}
	if err := recon.Take(1); err != nil {
		return "", err
	}

	// A cookie jar on the client is what captures the session: every
	// Set-Cookie the response carries is taken under the rules a browser
	// applies, and the jar holds the state of it.
	cookies, err := cookiejar.New(nil)
	if err != nil {
		return "", fmt.Errorf("create the cookie jar: %w", err)
	}
	client := &http.Client{
		Transport: netguard.Transport(),
		Jar:       cookies,
		Timeout:   time.Duration(timeout) * time.Second,
		// A redirect is a result, not a hop: the gate applied to the URL
		// that was asked for, and following a 3xx would move the request to
		// a host nobody authorized. The cookies of the 3xx are still
		// captured, because that is where a login often puts them.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	var payload io.Reader
	if body != "" {
		payload = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, parsed.String(), payload)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	for header, value := range headers {
		req.Header.Set(header, value)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", authUserAgent)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("login request failed: %w", err)
	}
	defer resp.Body.Close()
	// The body is read and dropped. It is bounded because a login response
	// can be a page, and it is never printed because it can be a token.
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxAuthBodyBytes)); err != nil {
		return "", fmt.Errorf("read the login response: %w", err)
	}

	captured := captureCookies(resp, parsed, cookies.Cookies(parsed))
	store, err := jar.Open(authSessionDir)
	if err != nil {
		return "", err
	}
	sess := jar.Session{
		Name:    name,
		Cookies: captured,
		Headers: replayableHeaders(headers),
		Meta: map[string]string{
			"login_url":    parsed.String(),
			"login_method": method,
			"logged_in_at": time.Now().UTC().Format(time.RFC3339),
			"login_status": fmt.Sprintf("%d", resp.StatusCode),
		},
	}
	if err := store.Save(sess); err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "session %q stored\n", name)
	fmt.Fprintf(&b, "status %d\n", resp.StatusCode)
	if names := cookieNames(captured); len(names) > 0 {
		fmt.Fprintf(&b, "cookies captured: %s (%d)\n", strings.Join(names, ", "), len(names))
	} else {
		b.WriteString("cookies captured: none\n")
		b.WriteString("note: the response set no cookie, so a replay of this session stays unauthenticated\n")
	}
	if location := resp.Header.Get("Location"); location != "" {
		// Clipped and stripped: a Location is a target's own text, and it
		// goes into the answer the operator reads.
		fmt.Fprintf(&b, "redirect %s (not followed)\n", authClip(cookieText(location), 160))
	}
	fmt.Fprintf(&b, "session file: %s\n", filepath.Join(store.Dir(), name+".json"))
	return b.String(), nil
}

// ---------------------------------------------------------------------------
// auth_replay
// ---------------------------------------------------------------------------

// AuthReplayTool replays a request with a stored session.
type AuthReplayTool struct{}

func (a *AuthReplayTool) Name() string { return "auth_replay" }

func (a *AuthReplayTool) Description() string {
	return "Replay a request with a stored session. The session's cookies are attached to the URL you give, subject to the real cookie rules (domain, path, Secure, expiry), and the session's stored headers - an Authorization header, for example - are merged in, with your own headers winning. The request is sent as an ordinary probe, so it is scope-gated, metered against the engagement rate budget and leaves a receipt in the recon trail. Returns the status, the elapsed time, the response size, the sha256 prefix, the receipt id and the bounded response body head. Cookie values are never printed."
}

func (a *AuthReplayTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"session": map[string]any{"type": "string", "description": "Name of the stored session to replay, as stored by auth_login"},
			"url":     map[string]any{"type": "string", "description": "Absolute http:// or https:// URL to request with the session"},
			"method":  map[string]any{"type": "string", "description": "HTTP method, default GET. One of GET, POST, HEAD, OPTIONS, PUT, PATCH, DELETE."},
			"body":    map[string]any{"additionalProperties": true, "description": "Request body for POST, PUT or PATCH: either a string, or a JSON object/array which is serialized and sent as application/json. Capped at 64k"},
			"headers": map[string]any{"type": "object", "additionalProperties": true, "description": "Request headers as name/value pairs; they win over the session's stored headers"},
			"timeout": map[string]any{"type": "integer", "description": "Request timeout in seconds, default 15, maximum 120"},
		},
		"required": []string{"session", "url"},
	}
}

func (a *AuthReplayTool) Translate(args map[string]any) (operation.Spec, error) {
	name, err := authNameArg(args["session"])
	if err != nil {
		return operation.Spec{}, fmt.Errorf("session is required: %w", err)
	}
	rawURL, err := authURLArg(args["url"])
	if err != nil {
		return operation.Spec{}, err
	}
	method, err := authMethodArg(args["method"], http.MethodGet)
	if err != nil {
		return operation.Spec{}, err
	}
	body, bodyIsJSON, err := recon.NormalizeBody(args["body"])
	if err != nil {
		return operation.Spec{}, err
	}
	headers, err := authHeaders(args["headers"])
	if err != nil {
		return operation.Spec{}, err
	}
	if bodyIsJSON && body != "" && !authHasHeader(headers, "Content-Type") {
		if headers == nil {
			headers = make(map[string]string, 1)
		}
		headers["Content-Type"] = "application/json"
	}
	out := map[string]any{
		"session": name,
		"url":     rawURL,
		"method":  method,
		"timeout": authTimeoutArg(args["timeout"]),
	}
	if len(headers) > 0 {
		out["headers"] = headers
	}
	if body != "" {
		out["body"] = body
	}
	return operation.Spec{Type: "auth_replay", Args: out}, nil
}

func (a *AuthReplayTool) TranslateResult(op operation.Operation) string { return op.Output }

func (a *AuthReplayTool) Run(spec operation.Spec) (string, error) {
	return a.RunContext(context.Background(), spec)
}

func (a *AuthReplayTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	name, err := authNameArg(spec.Args["session"])
	if err != nil {
		return "", fmt.Errorf("session is required: %w", err)
	}
	rawURL, err := authURLArg(spec.Args["url"])
	if err != nil {
		return "", err
	}
	method, err := authMethodArg(spec.Args["method"], http.MethodGet)
	if err != nil {
		return "", err
	}
	body, err := authBodyArg(spec.Args["body"])
	if err != nil {
		return "", err
	}
	callerHeaders, err := authHeaders(spec.Args["headers"])
	if err != nil {
		return "", err
	}
	timeout := authTimeoutArg(spec.Args["timeout"])

	store, err := jar.Open(authSessionDir)
	if err != nil {
		return "", err
	}
	sess, err := store.Load(name)
	if err != nil {
		// A missing session is a name the model can fix, so the error says
		// which names exist.
		return "", fmt.Errorf("%w; stored sessions: %s", err, storedSessionNames(store))
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("url is not valid: %w", err)
	}
	// Same gate, same budget, same order as the login: a refused replay
	// never reaches the target.
	if err := recon.Gate(rawURL); err != nil {
		return "", fmt.Errorf("refused, not sent: %w", err)
	}
	if err := recon.Take(1); err != nil {
		return "", err
	}

	// The session's stored headers first, the caller's over them: the
	// caller is answering now and knows more than the login did.
	headers := make(map[string]string, len(sess.Headers)+len(callerHeaders)+1)
	for header, value := range sess.Headers {
		headers[header] = value
	}
	for header, value := range callerHeaders {
		headers[header] = value
	}
	// The session's cookies go on after the merge: the point of the replay
	// is the session, and a caller header must not quietly replace it.
	if cookie := jar.CookieHeader(sess, parsed); cookie != "" {
		headers["Cookie"] = cookie
	}

	res, err := recon.Probe(ctx, recon.Request{
		URL:     rawURL,
		Method:  method,
		Headers: headers,
		Body:    body,
		Timeout: timeout,
	})
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", res.Method, res.URL)
	fmt.Fprintf(&b, "status %d in %dms, body %dB, sha256:%s\n",
		res.Status, res.ElapsedMS, res.BodyBytes, shaPrefix(res.SHA256))
	if res.ReceiptID != "" {
		fmt.Fprintf(&b, "receipt %s\n", res.ReceiptID)
	}
	if res.RedirectedTo != "" {
		fmt.Fprintf(&b, "redirect %s (not followed)\n", authClip(cookieText(res.RedirectedTo), 160))
	}
	if head := strings.TrimSpace(res.BodyHead); head != "" {
		fmt.Fprintf(&b, "body head:\n%s\n", authClip(head, maxAuthHeadRunes))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// ---------------------------------------------------------------------------
// auth_sessions
// ---------------------------------------------------------------------------

// AuthSessionsTool lists or deletes the stored sessions.
type AuthSessionsTool struct{}

func (a *AuthSessionsTool) Name() string { return "auth_sessions" }

func (a *AuthSessionsTool) Description() string {
	return "List or delete the stored sessions. Listing reports each session's name, its age and the names of the cookies it holds, never the cookie values. Deleting removes the session file from disk."
}

func (a *AuthSessionsTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode": map[string]any{"type": "string", "enum": []string{"list", "delete"}, "description": "list (default) shows the stored sessions; delete removes the session named"},
			"name": map[string]any{"type": "string", "description": "Name of the session to delete. Required for mode delete."},
		},
	}
}

func (a *AuthSessionsTool) Translate(args map[string]any) (operation.Spec, error) {
	mode := "list"
	if raw, ok := args["mode"].(string); ok && strings.TrimSpace(raw) != "" {
		mode = strings.ToLower(strings.TrimSpace(raw))
	}
	switch mode {
	case "list", "delete":
	default:
		return operation.Spec{}, fmt.Errorf("mode %q is not one of list, delete", mode)
	}
	out := map[string]any{"mode": mode}
	if mode == "delete" {
		name, err := authNameArg(args["name"])
		if err != nil {
			return operation.Spec{}, fmt.Errorf("name is required to delete a session: %w", err)
		}
		out["name"] = name
	}
	return operation.Spec{Type: "auth_sessions", Args: out}, nil
}

func (a *AuthSessionsTool) TranslateResult(op operation.Operation) string { return op.Output }

func (a *AuthSessionsTool) Run(spec operation.Spec) (string, error) {
	return a.RunContext(context.Background(), spec)
}

func (a *AuthSessionsTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	mode, _ := spec.Args["mode"].(string)
	if mode == "" {
		mode = "list"
	}
	store, err := jar.Open(authSessionDir)
	if err != nil {
		return "", err
	}
	switch mode {
	case "list":
		sessions, err := store.List()
		if err != nil {
			return "", err
		}
		if len(sessions) == 0 {
			return "no sessions stored", nil
		}
		lines := make([]string, 0, len(sessions))
		for _, sess := range sessions {
			age := time.Since(sess.CreatedAt)
			if sess.CreatedAt.IsZero() {
				age = 0
			}
			names := cookieNames(sess.Cookies)
			if len(names) == 0 {
				names = []string{"none"}
			}
			lines = append(lines, fmt.Sprintf("%s  age %s  cookies: %s", sess.Name, humanAge(age), strings.Join(names, ", ")))
		}
		return strings.Join(lines, "\n"), nil
	case "delete":
		name, err := authNameArg(spec.Args["name"])
		if err != nil {
			return "", fmt.Errorf("name is required to delete a session: %w", err)
		}
		if err := store.Delete(name); err != nil {
			return "", err
		}
		return fmt.Sprintf("deleted session %q", name), nil
	default:
		return "", fmt.Errorf("mode %q is not one of list, delete", mode)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// authNameArg reads and validates a session name.
func authNameArg(v any) (string, error) {
	name, _ := v.(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("a name is required")
	}
	if err := jar.ValidateName(name); err != nil {
		return "", err
	}
	return name, nil
}

// authURLArg reads an absolute http(s) URL. The scheme floor is here as well
// as in the gate, because the tool is independently callable.
func authURLArg(v any) (string, error) {
	raw, _ := v.(string)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("url is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("url is not valid: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if (scheme != "http" && scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("url must be an absolute http:// or https:// url")
	}
	return raw, nil
}

// authMethodArg normalises the method and refuses anything outside the
// probe's whitelist, so a replay cannot become a request the probe would
// not have made.
func authMethodArg(v any, fallback string) (string, error) {
	raw, _ := v.(string)
	method := strings.ToUpper(strings.TrimSpace(raw))
	if method == "" {
		return fallback, nil
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodHead, http.MethodOptions,
		http.MethodPut, http.MethodPatch, http.MethodDelete:
		return method, nil
	}
	return "", fmt.Errorf("method %q is not one of GET, POST, HEAD, OPTIONS, PUT, PATCH, DELETE", method)
}

// authTimeoutArg normalises the JSON-decoded timeout to seconds. A value
// outside the probe's range is the default rather than an argument dispute:
// the model asked for a request.
func authTimeoutArg(v any) int {
	switch t := v.(type) {
	case int:
		if t > 0 && t <= authMaxTimeout {
			return t
		}
	case float64:
		if t > 0 && t <= authMaxTimeout {
			return int(t)
		}
	}
	return authDefaultTimeout
}

// authHeaders reads the headers in the shapes they arrive in: the map of
// strings a translated spec carries, and the map of anything a JSON decoded
// spec carries.
func authHeaders(v any) (map[string]string, error) {
	switch headers := v.(type) {
	case nil:
		return nil, nil
	case map[string]string:
		return headers, nil
	case map[string]any:
		out := make(map[string]string, len(headers))
		for name, value := range headers {
			switch typed := value.(type) {
			case string:
				out[name] = typed
			case bool, int, int64, float64:
				out[name] = fmt.Sprint(typed)
			default:
				return nil, fmt.Errorf("header %q value must be a string, number or boolean", name)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("headers must be an object of name/value pairs")
	}
}

// authHasHeader reports whether a header is already set, ignoring case so
// the automatic Content-Type never duplicates an explicit one.
func authHasHeader(headers map[string]string, name string) bool {
	for key := range headers {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

// authBodyArg reads a request body in either shape: the string a translated
// spec carries, or the object a model wrote.
func authBodyArg(v any) (string, error) {
	body, _, err := recon.NormalizeBody(v)
	return body, err
}

// replayableHeaders picks the headers worth keeping out of a login's
// request headers. A User-Agent or a Content-Type would be wrong to replay,
// and a Cookie belongs to the jar, not here.
func replayableHeaders(headers map[string]string) map[string]string {
	out := make(map[string]string, len(replayHeaders))
	for name, value := range headers {
		lower := strings.ToLower(name)
		for _, keep := range replayHeaders {
			if lower == keep {
				out[name] = value
				break
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// captureCookies reads the cookies a login response set.
//
// The client carries a cookie jar, so every Set-Cookie the response carried
// was captured under the rules a browser applies, and the jar holds the
// session state. Go's jar drops the attributes a replay needs (domain, path,
// expiry, the secure flag) from its public view, so the attributes are read
// off the response and the acceptance rule is applied here: a cookie whose
// domain does not cover the host that set it is not a session cookie, a
// cookie that is being deleted is not stored at all, and control characters
// are stripped from a name and a value before either can be replayed into a
// request header.
//
// The jar's own view of the session is added as a fallback, so a cookie it
// kept that the response did not describe is still part of the session.
func captureCookies(resp *http.Response, u *url.URL, held []*http.Cookie) []jar.Cookie {
	host := strings.ToLower(u.Hostname())
	raw := resp.Cookies()
	out := make([]jar.Cookie, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	now := time.Now()
	for _, c := range raw {
		if c.MaxAge < 0 {
			// Max-Age=0 is a deletion, not a cookie.
			continue
		}
		name := cookieText(c.Name)
		if name == "" {
			continue
		}
		domain := strings.TrimSpace(c.Domain)
		switch {
		case domain == "":
			// No domain attribute: the cookie belongs to the host that set it.
			domain = host
		default:
			if !domainCovers(strings.TrimPrefix(strings.ToLower(domain), "."), host) {
				continue
			}
		}
		path := c.Path
		if path == "" {
			path = defaultCookiePath(u.Path)
		}
		expires := c.Expires
		if c.MaxAge > 0 {
			expires = now.Add(time.Duration(c.MaxAge) * time.Second)
		}
		value := cookieText(c.Value)
		seen[name+"="+value] = true
		out = append(out, jar.Cookie{
			Name:     name,
			Value:    value,
			Domain:   domain,
			Path:     path,
			Expires:  expires,
			Secure:   c.Secure,
			HTTPOnly: c.HttpOnly,
		})
	}
	for _, c := range held {
		key := c.Name + "=" + c.Value
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, jar.Cookie{
			Name:   cookieText(c.Name),
			Value:  cookieText(c.Value),
			Domain: host,
			Path:   defaultCookiePath(u.Path),
		})
	}
	return out
}

// domainCovers reports whether a cookie domain covers a host. The direction
// matters: a cookie for api.example.com never covers example.com.
func domainCovers(domain, host string) bool {
	if domain == "" || host == "" {
		return false
	}
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// defaultCookiePath is the path a cookie without a Path attribute covers: the
// directory of the request path, the way a browser computes it.
func defaultCookiePath(requestPath string) string {
	if requestPath == "" || requestPath[0] != '/' {
		return "/"
	}
	idx := strings.LastIndex(requestPath, "/")
	if idx <= 0 {
		return "/"
	}
	return requestPath[:idx]
}

// cookieText strips the characters that must never reach a request header.
// A cookie name or value carrying a line break is header injection, and the
// server is not the one that has to be trusted with it.
func cookieText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// cookieNames lists the names of a set of cookies, in the order they were
// captured. The values are the credentials and they are not here.
func cookieNames(cookies []jar.Cookie) []string {
	names := make([]string, 0, len(cookies))
	for _, c := range cookies {
		names = append(names, c.Name)
	}
	return names
}

// storedSessionNames lists the names of the sessions on disk, for an error
// that has to tell the model which name to use.
func storedSessionNames(store *jar.Store) string {
	sessions, err := store.List()
	if err != nil || len(sessions) == 0 {
		return "none"
	}
	names := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		names = append(names, sess.Name)
	}
	return strings.Join(names, ", ")
}

// shaPrefix shortens a digest for the summary.
func shaPrefix(sum string) string {
	if len(sum) <= 12 {
		return sum
	}
	return sum[:12]
}

// authClip bounds a string in runes, so the cut lands on a character
// boundary and multibyte text stays decodable.
func authClip(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

// humanAge renders a duration the way a person would say it: "2h" is a fact
// about a session, "2h0m0s" is a stopwatch.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
