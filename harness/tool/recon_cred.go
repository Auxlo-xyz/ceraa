package tool

// recon_cred.go is the credential test wearing a tool face.
//
// The gap it closes: the recon layer could log in once and replay, but it
// could not test a list of credentials, could not tell a locked account from
// a wrong password, and could not turn a successful login into a session it
// could keep using. That is the difference between testing one account and
// testing an account list, which is where a lot of real findings start.
//
// One tool, three modes, because they are three questions: test the
// credentials (spray), what could the accounts be called (usernames), and
// how many passwords would be tried (passwords).
//
// Two rules shape the output:
//
//   - A password never appears in it. The report of a credential test gets
//     pasted into a disclosure, and a disclosure that carries the password it
//     found is a second copy of the secret. The username is printed; the
//     password is not.
//   - The password list is never printed either. A password list in a chat
//     transcript is a liability, so the passwords mode reports a count.

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"ceraa/harness/creds"
	"ceraa/harness/jar"
	"ceraa/harness/operation"
	"ceraa/harness/recon"
	"ceraa/internal/paths"
)

// The defaults and the bounds the tool clamps to.
const (
	credDefaultMaxPerUser = 3
	credDefaultTimeout    = 15
	credMaxPerUser        = 20
	credMaxTimeout        = 120
	credMaxConcurrency    = 8
	credMaxDelayMS        = 60000
	credMaxListItems      = 200
	credMaxMarkers        = 16
	// credMaxOutcomeLines bounds how many attempts the tool prints. A
	// spray of five hundred credentials is five hundred lines, and a chat
	// message that carries five hundred lines is a message nobody reads.
	// The summary carries the counts.
	credMaxOutcomeLines = 50
	// credMaxUsernameLines bounds the candidate username list for the same
	// reason.
	credMaxUsernameLines = 60
)

// credSessionDir holds the sessions a successful credential produced.
//
// It is the same directory the login tool stores its sessions in, so a
// session found by a credential test can be replayed with auth_replay
// without the operator having to move anything.
var credSessionDir = filepath.Join(paths.DataDir(), "recon", "sessions")

// CredTool tests a list of credentials against a login endpoint.
type CredTool struct{}

func (c *CredTool) Name() string { return "cred" }

func (c *CredTool) Description() string {
	return "Test a list of credentials against a login endpoint. Every attempt is scope-gated and receipted. The passwords never appear in the output, a success is stored as a session auth_replay can use, and a per-account ceiling stops a spray that would lock accounts."
}

func (c *CredTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode":           map[string]any{"type": "string", "enum": []string{"spray", "usernames", "passwords"}, "description": "spray, usernames or passwords"},
			"url":            map[string]any{"type": "string", "description": "The login endpoint"},
			"method":         map[string]any{"type": "string", "description": "POST or GET, default POST"},
			"body":           map[string]any{"type": "string", "description": "Body template with {USERNAME} and {PASSWORD}"},
			"headers":        map[string]any{"type": "object", "additionalProperties": true, "description": "Request headers"},
			"username_field": map[string]any{"type": "string", "description": "Username field, default username"},
			"password_field": map[string]any{"type": "string", "description": "Password field, default password"},
			"success_marker": map[string]any{"type": "string", "description": "Text that only appears on success"},
			"success_field":  map[string]any{"type": "string", "description": "JSON field that decides success"},
			"failure_marker": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 16, "description": "Text that means the credentials were rejected"},
			"locked_marker":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 16, "description": "Text that means the account is locked"},
			"domain":         map[string]any{"type": "string", "description": "Domain to derive usernames from"},
			"users":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 200, "description": "Usernames to try"},
			"passwords":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 200, "description": "Passwords to try"},
			"combo_file":     map[string]any{"type": "string", "description": "Path to a user:pass list"},
			"max_per_user":   map[string]any{"type": "integer", "minimum": 1, "maximum": 20, "description": "Attempts per account, default 3"},
			"delay_ms":       map[string]any{"type": "integer", "minimum": 0, "maximum": 60000, "description": "Pause between attempts, ms"},
			"concurrency":    map[string]any{"type": "integer", "minimum": 1, "maximum": 8, "description": "Parallel attempts, default 1"},
			"timeout":        map[string]any{"type": "integer", "minimum": 1, "maximum": 120, "description": "Per-request timeout, seconds"},
			"session_name":   map[string]any{"type": "string", "description": "Session name for a successful login"},
		},
		"required": []string{},
	}
}

// Translate validates the shape and re-emits the canonical argument map.
func (c *CredTool) Translate(args map[string]any) (operation.Spec, error) {
	mode := "spray"
	if raw := strings.TrimSpace(credStringArg(args["mode"])); raw != "" {
		mode = strings.ToLower(raw)
	}
	switch mode {
	case "spray", "usernames", "passwords":
	default:
		return operation.Spec{}, fmt.Errorf("mode %q is not one of spray, usernames, passwords", mode)
	}

	out := map[string]any{"mode": mode}
	if mode == "spray" {
		rawURL := strings.TrimSpace(credStringArg(args["url"]))
		if rawURL == "" {
			return operation.Spec{}, fmt.Errorf("mode spray needs the url of the login endpoint")
		}
		if _, err := credURLArg(rawURL); err != nil {
			return operation.Spec{}, err
		}
		out["url"] = rawURL
		out["method"] = credMethodArg(args["method"])
		body, _, err := recon.NormalizeBody(args["body"])
		if err != nil {
			return operation.Spec{}, err
		}
		if body != "" {
			out["body"] = body
		}
		headers, err := credHeaders(args["headers"])
		if err != nil {
			return operation.Spec{}, err
		}
		if len(headers) > 0 {
			out["headers"] = headers
		}
		out["username_field"] = credStringArg(args["username_field"])
		out["password_field"] = credStringArg(args["password_field"])
		out["success_marker"] = credStringArg(args["success_marker"])
		out["success_field"] = credStringArg(args["success_field"])
		if markers := credStringListArg(args["failure_marker"], credMaxMarkers); len(markers) > 0 {
			out["failure_marker"] = markers
		}
		if markers := credStringListArg(args["locked_marker"], credMaxMarkers); len(markers) > 0 {
			out["locked_marker"] = markers
		}
		out["max_per_user"] = credIntArg(args["max_per_user"], credDefaultMaxPerUser)
		out["delay_ms"] = credIntArg(args["delay_ms"], 0)
		out["concurrency"] = credIntArg(args["concurrency"], 1)
		out["timeout"] = credIntArg(args["timeout"], credDefaultTimeout)
	}
	if domain := strings.TrimSpace(credStringArg(args["domain"])); domain != "" {
		out["domain"] = domain
	}
	if users := credStringListArg(args["users"], credMaxListItems); len(users) > 0 {
		out["users"] = users
	}
	if passwords := credStringListArg(args["passwords"], credMaxListItems); len(passwords) > 0 {
		out["passwords"] = passwords
	}
	if combo := strings.TrimSpace(credStringArg(args["combo_file"])); combo != "" {
		out["combo_file"] = combo
	}
	if name := strings.TrimSpace(credStringArg(args["session_name"])); name != "" {
		out["session_name"] = name
	}
	return operation.Spec{Type: "cred", Args: out}, nil
}

func (c *CredTool) TranslateResult(op operation.Operation) string { return op.Output }

func (c *CredTool) Run(spec operation.Spec) (string, error) {
	return c.RunContext(context.Background(), spec)
}

func (c *CredTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(credStringArg(spec.Args["mode"])))
	if mode == "" {
		mode = "spray"
	}
	switch mode {
	case "usernames":
		return credUsernames(spec)
	case "passwords":
		return credPasswords(spec)
	case "spray":
		return credSpray(ctx, spec)
	default:
		return "", fmt.Errorf("mode %q is not one of spray, usernames, passwords", mode)
	}
}

// credUsernames prints the candidate usernames, one per line, with the
// source of each one.
//
// The sources travel with the names because a list with no provenance is a
// list nobody can reason about: the model has to know which names were
// derived from the domain and which ones it supplied.
func credUsernames(spec operation.Spec) (string, error) {
	domain := credDomain(spec)
	if domain == "" {
		return "", fmt.Errorf("mode usernames needs a domain, or a url to take the host from")
	}
	candidates := creds.UsernameCandidates(domain, credStringListArg(spec.Args["users"], credMaxListItems))
	var b strings.Builder
	fmt.Fprintf(&b, "cred: %d candidate usernames for %s\n", len(candidates), domain)
	shown := candidates
	if len(shown) > credMaxUsernameLines {
		shown = shown[:credMaxUsernameLines]
	}
	for _, candidate := range shown {
		fmt.Fprintf(&b, "%s  (%s)\n", candidate.Username, candidate.Source)
	}
	if len(candidates) > len(shown) {
		fmt.Fprintf(&b, "... and %d more candidates not shown\n", len(candidates)-len(shown))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// credPasswords reports how many passwords would be tried and prints none
// of them.
//
// A password list in a chat transcript is a liability, and the count is
// enough for the model to decide whether the list is worth spending the
// engagement's budget on.
func credPasswords(spec operation.Spec) (string, error) {
	explicit := credStringListArg(spec.Args["passwords"], credMaxListItems)
	if len(explicit) > 0 {
		return fmt.Sprintf("cred: %d explicit passwords (the list is not printed: a password list in a transcript is a liability)", len(explicit)), nil
	}
	domain := credDomain(spec)
	if domain == "" {
		return "", fmt.Errorf("mode passwords needs a domain, or a url to take the host from")
	}
	passwords := creds.Passwords([]string{domain})
	return fmt.Sprintf("cred: %d candidate passwords for %s (the list is not printed: a password list in a transcript is a liability)", len(passwords), domain), nil
}

// credSpray runs the credential test and renders what came back.
func credSpray(ctx context.Context, spec operation.Spec) (string, error) {
	rawURL := strings.TrimSpace(credStringArg(spec.Args["url"]))
	if rawURL == "" {
		return "", fmt.Errorf("mode spray needs the url of the login endpoint")
	}
	body, _, err := recon.NormalizeBody(spec.Args["body"])
	if err != nil {
		return "", err
	}
	headers, err := credHeaders(spec.Args["headers"])
	if err != nil {
		return "", err
	}
	credentials, err := credCredentials(spec, rawURL)
	if err != nil {
		return "", err
	}
	if len(credentials) == 0 {
		return "", fmt.Errorf("there are no credentials to try: give users, passwords, a combo_file or a domain to derive them from")
	}

	store, err := jar.Open(credSessionDir)
	if err != nil {
		return "", err
	}

	result, err := creds.SprayDetailed(ctx, creds.SprayOptions{
		URL:                rawURL,
		Method:             credMethodArg(spec.Args["method"]),
		Body:               body,
		Headers:            headers,
		UsernameField:      credStringArg(spec.Args["username_field"]),
		PasswordField:      credStringArg(spec.Args["password_field"]),
		SuccessMarker:      credStringArg(spec.Args["success_marker"]),
		SuccessField:       credStringArg(spec.Args["success_field"]),
		FailureMarkers:     credStringListArg(spec.Args["failure_marker"], credMaxMarkers),
		LockedMarkers:      credStringListArg(spec.Args["locked_marker"], credMaxMarkers),
		MaxAttemptsPerUser: credIntArg(spec.Args["max_per_user"], credDefaultMaxPerUser),
		DelayMS:            credIntArg(spec.Args["delay_ms"], 0),
		Concurrency:        credIntArg(spec.Args["concurrency"], 1),
		Timeout:            credIntArg(spec.Args["timeout"], credDefaultTimeout),
		Credentials:        credentials,
		Store:              store,
		SessionName:        credStringArg(spec.Args["session_name"]),
	})
	if err != nil {
		return "", err
	}
	return formatCredSpray(result, rawURL), nil
}

// formatCredSpray renders the run: one bounded line per attempt, the
// summary, and the sessions that were stored.
//
// The password never appears in it. The outcome renders the username, the
// status and the reason, and the evidence is already scrubbed.
func formatCredSpray(result creds.Result, rawURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "cred: spray %s\n", rawURL)
	shown := result.Outcomes
	if len(shown) > credMaxOutcomeLines {
		shown = shown[:credMaxOutcomeLines]
	}
	for _, outcome := range shown {
		fmt.Fprintf(&b, "%s\n", outcome.String())
	}
	if len(result.Outcomes) > len(shown) {
		fmt.Fprintf(&b, "... and %d more attempts not shown\n", len(result.Outcomes)-len(shown))
	}
	fmt.Fprintf(&b, "summary: %s\n", result.Summary.String())
	for _, name := range result.Summary.Sessions {
		fmt.Fprintf(&b, "session %q stored\n", name)
	}
	for _, message := range result.Summary.Errors {
		fmt.Fprintf(&b, "error: %s\n", message)
	}
	return strings.TrimRight(b.String(), "\n")
}

// credCredentials builds the list of pairs the spray tries.
//
// The order is the password first: one password against every account
// before the next password. The per-account ceiling is what keeps the test
// from locking the accounts, and the ceiling only works if the attempts of
// one account are spread across the list instead of running through it.
func credCredentials(spec operation.Spec, rawURL string) ([]creds.Credential, error) {
	if combo := strings.TrimSpace(credStringArg(spec.Args["combo_file"])); combo != "" {
		// A relative path from the model means the session workdir, not the
		// process directory, and the resolved path goes in the error so a
		// retry can correct itself.
		base, _ := spec.Args["workdir"].(string)
		resolved, err := ResolvePathFrom(base, combo)
		if err != nil {
			return nil, fmt.Errorf("combo file %s: %w", combo, err)
		}
		combos, err := creds.LoadCombos(resolved)
		if err != nil {
			return nil, err
		}
		return combos, nil
	}

	users := credStringListArg(spec.Args["users"], credMaxListItems)
	source := "wordlist"
	if len(users) == 0 {
		domain := credDomain(spec)
		users = creds.Usernames(domain, nil)
	} else {
		source = "explicit"
	}
	passwords := credStringListArg(spec.Args["passwords"], credMaxListItems)
	if len(passwords) == 0 {
		passwords = creds.Passwords([]string{credDomain(spec)})
	}

	out := make([]creds.Credential, 0, len(users)*len(passwords))
	for _, password := range passwords {
		for _, user := range users {
			out = append(out, creds.Credential{Username: user, Password: password, Source: source})
		}
	}
	return out, nil
}

// credDomain is the domain the candidates are derived from: the one the
// caller gave, or the host of the url.
func credDomain(spec operation.Spec) string {
	if domain := strings.TrimSpace(credStringArg(spec.Args["domain"])); domain != "" {
		return domain
	}
	rawURL := strings.TrimSpace(credStringArg(spec.Args["url"]))
	if rawURL == "" {
		return ""
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// credStringArg reads a string argument without the zero-value trap.
func credStringArg(v any) string {
	s, _ := v.(string)
	return s
}

// credStringListArg reads a list of strings in both the native and the
// JSON-decoded shape.
func credStringListArg(v any, max int) []string {
	switch typed := v.(type) {
	case []string:
		if len(typed) > max {
			typed = typed[:max]
		}
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, entry := range typed {
			if s, ok := entry.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		if len(out) > max {
			out = out[:max]
		}
		return out
	}
	return nil
}

// credIntArg reads an integer argument, falling back when the value is
// absent or unusable.
func credIntArg(v any, fallback int) int {
	switch typed := v.(type) {
	case int:
		if typed >= 0 {
			return typed
		}
	case float64:
		if typed >= 0 && typed == float64(int(typed)) {
			return int(typed)
		}
	}
	return fallback
}

// credMethodArg normalises the method. Only the two verbs a login has are
// allowed, so a credential test cannot become a mutation.
func credMethodArg(v any) string {
	method := strings.ToUpper(strings.TrimSpace(credStringArg(v)))
	if method == "" {
		return "POST"
	}
	return method
}

// credURLArg reads an absolute http(s) URL.
//
// The scheme floor is here as well as in the gate, because the tool is
// independently callable and a relative path is not a destination.
func credURLArg(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("url is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("url is not valid: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if (scheme != "http" && scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("url must be an absolute http:// or https:// url")
	}
	return trimmed, nil
}

// credHeaders reads the headers in the shapes they arrive in.
func credHeaders(v any) (map[string]string, error) {
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
