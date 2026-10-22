package tool

// recon_auth.go is the session jar wearing a tool face.
//
// The gap it closes: the recon layer could probe a URL but could not log in
// and keep the result, so every authenticated test - IDOR, BOLA, an admin
// panel, CSRF - was done by hand. A login here is one request through the
// guarded transport with a cookie jar attached, and what the response set
// becomes a named session on disk that later requests replay.
//
// One tool carries the whole workflow. The catalogue is the cached prefix of
// every request, so three entries for one capability are three schemas the
// model reads on every turn, and a mode argument says which of the four
// behaviours is meant: log in once, replay as often as the test needs, see
// what is stored, throw it away.
//
// The modes are not one mode with flags. Each has its own argument set, and
// a schema cannot make a field conditionally required, so the mode decides
// what the call must carry and the body refuses a call that is short of it.

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

// The four behaviours the one tool carries.
//
// They are separate modes rather than one mode with flags because each has
// its own argument set: a login names a session to store, a replay names a
// session to use, a delete names a session to remove.
const (
	authModeLogin  = "login"
	authModeReplay = "replay"
	authModeList   = "list"
	authModeDelete = "delete"
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
// auth
// ---------------------------------------------------------------------------

// AuthTool keeps a logged-in session.
type AuthTool struct{}

// The executor resolves a tool by its operation type and calls it through the
// context runner, so the two have to agree with the name.
var _ operation.ContextRunner = (*AuthTool)(nil)

// The three names the registry still registers are aliases of the merged
// tool, not tools of their own.
func (a *AuthTool) Name() string { return "auth" }

func (a *AuthTool) Description() string {
	return "Keep a logged-in session: log in and store it, replay a request with it, list or delete the stored sessions; the login and the replay are scope-gated and receipted, and cookie values never leave the machine."
}

func (a *AuthTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode": map[string]any{
				"type":        "string",
				"enum":        []string{authModeLogin, authModeReplay, authModeList, authModeDelete},
				"description": "login (default) stores a session, replay sends a request with one, list shows the stored sessions, delete removes one",
			},
			"name": map[string]any{
				"type":        "string",
				"description": "login: the name to store the session under; delete: the name of the session to remove",
			},
			"url": map[string]any{
				"type":        "string",
				"description": "login: the login endpoint; replay: the URL to request with the session",
			},
			"session": map[string]any{
				"type":        "string",
				"description": "replay: the name of the stored session to replay",
			},
			"method": map[string]any{
				"type":        "string",
				"description": "login: the method of the login request, default POST; replay: the method of the request, default GET",
			},
			"body": map[string]any{
				"additionalProperties": true,
				"description":          "login or replay: a string, or a JSON object/array which is serialized and sent as application/json. Capped at 64k",
			},
			"headers": map[string]any{
				"type":                 "object",
				"additionalProperties": true,
				"description":          "login or replay: request headers as name/value pairs; on replay they win over the session's stored headers",
			},
			"timeout": map[string]any{
				"type":        "integer",
				"description": "login or replay: request timeout in seconds, default 15, maximum 120",
			},
		},
		// Nothing is required by the schema: the mode decides. A name and a
		// url are required to log in, a session and a url to replay, and a
		// name to delete, and the tool body refuses the call that is short
		// of one, because a schema cannot make a field conditionally
		// required.
		"required": []string{},
	}
}

// Translate validates the shape against the mode and re-emits the canonical
// argument map.
func (a *AuthTool) Translate(args map[string]any) (operation.Spec, error) {
	mode, err := authMode(args["mode"])
	if err != nil {
		return operation.Spec{}, err
	}
	out := map[string]any{"mode": mode}

	switch mode {
	case authModeLogin:
		name, err := authNameArg(args["name"], "name", "to store a session")
		if err != nil {
			return operation.Spec{}, err
		}
		rawURL, err := authURLArg(args["url"], "to log in")
		if err != nil {
			return operation.Spec{}, err
		}
		request, err := authRequestArgs(args, http.MethodPost)
		if err != nil {
			return operation.Spec{}, err
		}
		out["name"] = name
		out["url"] = rawURL
		for key, value := range request {
			out[key] = value
		}

	case authModeReplay:
		session, err := authNameArg(args["session"], "session", "to replay")
		if err != nil {
			return operation.Spec{}, err
		}
		rawURL, err := authURLArg(args["url"], "to replay")
		if err != nil {
			return operation.Spec{}, err
		}
		request, err := authRequestArgs(args, http.MethodGet)
		if err != nil {
			return operation.Spec{}, err
		}
		out["session"] = session
		out["url"] = rawURL
		for key, value := range request {
			out[key] = value
		}

	case authModeDelete:
		name, err := authNameArg(args["name"], "name", "to delete a session")
		if err != nil {
			return operation.Spec{}, err
		}
		out["name"] = name

	case authModeList:
	}

	return operation.Spec{Type: "auth", Args: out}, nil
}

func (a *AuthTool) TranslateResult(op operation.Operation) string { return op.Output }

func (a *AuthTool) Run(spec operation.Spec) (string, error) {
	return a.RunContext(context.Background(), spec)
}

// RunContext carries the mode to the behaviour it names.
func (a *AuthTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	mode, err := authMode(spec.Args["mode"])
	if err != nil {
		return "", err
	}
	switch mode {
	case authModeLogin:
		return authRunLogin(ctx, spec)
	case authModeReplay:
		return authRunReplay(ctx, spec)
	case authModeList:
		return authListSessions()
	case authModeDelete:
		return authDeleteSession(spec)
	}
	return "", fmt.Errorf("mode %q is not one of login, replay, list, delete", mode)
}

// ---------------------------------------------------------------------------
// login
// ---------------------------------------------------------------------------

// authRunLogin performs the login and stores the session it produced.
func authRunLogin(ctx context.Context, spec operation.Spec) (string, error) {
	name, err := authNameArg(spec.Args["name"], "name", "to store a session")
	if err != nil {
		return "", err
	}
	rawURL, err := authURLArg(spec.Args["url"], "to log in")
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
// replay
// ---------------------------------------------------------------------------

// authRunReplay replays a request with a stored session.
func authRunReplay(ctx context.Context, spec operation.Spec) (string, error) {
	name, err := authNameArg(spec.Args["session"], "session", "to replay")
	if err != nil {
		return "", err
	}
	rawURL, err := authURLArg(spec.Args["url"], "to replay")
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
// list and delete
// ---------------------------------------------------------------------------

// authListSessions reports the stored sessions as a catalogue: names, ages
// and cookie names, never a cookie value.
func authListSessions() (string, error) {
	store, err := jar.Open(authSessionDir)
	if err != nil {
		return "", err
	}
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
}

// authDeleteSession removes a stored session file.
func authDeleteSession(spec operation.Spec) (string, error) {
	name, err := authNameArg(spec.Args["name"], "name", "to delete a session")
	if err != nil {
		return "", err
	}
	store, err := jar.Open(authSessionDir)
	if err != nil {
		return "", err
	}
	if err := store.Delete(name); err != nil {
		return "", err
	}
	return fmt.Sprintf("deleted session %q", name), nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// authMode reads the mode argument.
//
// The default is login: the tool exists to keep a session, and a login is
// what starts one.
func authMode(value any) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(authString(value)))
	if mode == "" {
		return authModeLogin, nil
	}
	switch mode {
	case authModeLogin, authModeReplay, authModeList, authModeDelete:
		return mode, nil
	}
	return "", fmt.Errorf("mode %q is not one of login, replay, list, delete", mode)
}

// authString reads a string argument, treating any other type as absent.
func authString(value any) string {
	text, _ := value.(string)
	return text
}

// authNameArg reads and validates a session name.
//
// The label names the argument the model used, so an error says which one was
// missing rather than repeating that a name is missing.
func authNameArg(v any, label, purpose string) (string, error) {
	name := strings.TrimSpace(authString(v))
	if name == "" {
		return "", fmt.Errorf("%s is required %s", label, purpose)
	}
	if err := jar.ValidateName(name); err != nil {
		return "", err
	}
	return name, nil
}

// authURLArg reads an absolute http(s) URL. The scheme floor is here as well
// as in the gate, because the tool is independently callable.
func authURLArg(v any, purpose string) (string, error) {
	raw := strings.TrimSpace(authString(v))
	if raw == "" {
		return "", fmt.Errorf("url is required %s", purpose)
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
	method := strings.ToUpper(strings.TrimSpace(authString(v)))
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

// authRequestArgs builds the request arguments a login and a replay share:
// the method, the body, the headers and the timeout.
//
// The method default is the caller's, because the two modes differ: a login
// posts and a replay reads.
func authRequestArgs(args map[string]any, defaultMethod string) (map[string]any, error) {
	method, err := authMethodArg(args["method"], defaultMethod)
	if err != nil {
		return nil, err
	}
	body, bodyIsJSON, err := recon.NormalizeBody(args["body"])
	if err != nil {
		return nil, err
	}
	headers, err := authHeaders(args["headers"])
	if err != nil {
		return nil, err
	}
	if bodyIsJSON && body != "" && !authHasHeader(headers, "Content-Type") {
		if headers == nil {
			headers = make(map[string]string, 1)
		}
		headers["Content-Type"] = "application/json"
	}
	out := map[string]any{
		"method":  method,
		"timeout": authTimeoutArg(args["timeout"]),
	}
	if len(headers) > 0 {
		out["headers"] = headers
	}
	if body != "" {
		out["body"] = body
	}
	return out, nil
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
