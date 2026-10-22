package tool

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ceraa/harness/act"
	"ceraa/harness/operation"
	"ceraa/internal/paths"
)

// The bounds of the roast.
const (
	// kerbMaxAccounts is the ceiling on the accounts one roast may ask
	// for.
	kerbMaxAccounts = 1000
	// kerbMaxPasswords is the ceiling on the candidate passwords.
	kerbMaxPasswords = 10000
	// kerbDefaultTimeout is the per-request timeout in seconds.
	kerbDefaultTimeout = 15
	// kerbMaxTimeout is the ceiling on the timeout in seconds.
	kerbMaxTimeout = 120
	// kerbMaxNameLength bounds the name of the result file.
	kerbMaxNameLength = 128
)

// KerbTool is the model-facing face of the Kerberos roast: ask a KDC for
// service tickets, then check candidate passwords against them offline.
//
// It only shapes arguments and renders answers. The protocol lives in
// harness/act, so the tool and the engine cannot drift apart.
type KerbTool struct{}

func (k *KerbTool) Name() string { return "kerb" }

func (k *KerbTool) Description() string {
	return "Roast Kerberos accounts: ask a KDC for an AS-REP per account and check candidate passwords against the reply offline. A validated password is written to a file at 0600 and never printed."
}

func (k *KerbTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kdc":   map[string]any{"type": "string", "description": "The KDC, as host or host:port"},
			"realm": map[string]any{"type": "string", "description": "The realm, as EXAMPLE.COM"},
			"accounts": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"maxItems":    kerbMaxAccounts,
				"description": "The accounts to ask tickets for",
			},
			"passwords": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"maxItems":    kerbMaxPasswords,
				"description": "The candidate passwords",
			},
			"timeout": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"maximum":     120,
				"description": "Timeout in seconds, default 15",
			},
			"out": map[string]any{
				"type":        "string",
				"description": "The file the validated passwords are written to",
			},
		},
		"required": []string{"kdc", "realm"},
	}
}

// Translate validates the shape and re-emits the canonical argument map.
//
// The candidate passwords are carried in the arguments because the check
// needs them, and they are carried nowhere else: the tool never renders a
// password, and a validated one goes to a file the operator opens.
func (k *KerbTool) Translate(args map[string]any) (operation.Spec, error) {
	kdc := strings.TrimSpace(kerbStringArg(args["kdc"]))
	if kdc == "" {
		return operation.Spec{}, fmt.Errorf("kdc is required")
	}
	realm := strings.TrimSpace(kerbStringArg(args["realm"]))
	if realm == "" {
		return operation.Spec{}, fmt.Errorf("realm is required")
	}
	accounts := kerbStringListArg(args["accounts"])
	if len(accounts) > kerbMaxAccounts {
		return operation.Spec{}, fmt.Errorf("accounts exceeded: %d given, maximum %d", len(accounts), kerbMaxAccounts)
	}
	passwords := kerbStringListArg(args["passwords"])
	if len(passwords) > kerbMaxPasswords {
		return operation.Spec{}, fmt.Errorf("passwords exceeded: %d given, maximum %d", len(passwords), kerbMaxPasswords)
	}
	timeout := kerbDefaultTimeout
	if value, ok := kerbIntArg(args["timeout"]); ok && value > 0 {
		timeout = value
	}
	if timeout > kerbMaxTimeout {
		timeout = kerbMaxTimeout
	}
	out := map[string]any{
		"kdc":     kdc,
		"realm":   realm,
		"timeout": timeout,
	}
	if len(accounts) > 0 {
		out["accounts"] = accounts
	}
	if len(passwords) > 0 {
		out["passwords"] = passwords
	}
	if name := strings.TrimSpace(kerbStringArg(args["out"])); name != "" {
		if err := kerbValidateName(name); err != nil {
			return operation.Spec{}, err
		}
		out["out"] = name
	}
	return operation.Spec{Type: "kerb", Args: out}, nil
}

func (k *KerbTool) TranslateResult(op operation.Operation) string { return op.Output }

func (k *KerbTool) Run(spec operation.Spec) (string, error) {
	return k.RunContext(context.Background(), spec)
}

// RunContext roasts and renders.
//
// The order is the house order: the scope record decides whether the KDC may
// be contacted at all, and only then does anything dial. A refusal therefore
// means nothing was sent.
func (k *KerbTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	kdc := strings.TrimSpace(kerbStringArg(spec.Args["kdc"]))
	if kdc == "" {
		return "", fmt.Errorf("kdc is required")
	}
	realm := strings.TrimSpace(kerbStringArg(spec.Args["realm"]))
	if realm == "" {
		return "", fmt.Errorf("realm is required")
	}
	accounts := kerbStringListArg(spec.Args["accounts"])
	passwords := kerbStringListArg(spec.Args["passwords"])
	if len(accounts) == 0 {
		return "", fmt.Errorf("accounts is required: there is nothing to roast without an account to ask a ticket for")
	}
	if len(passwords) == 0 {
		return "", fmt.Errorf("passwords is required: there is nothing to check without a candidate")
	}
	timeout, _ := kerbIntArg(spec.Args["timeout"])
	if timeout <= 0 {
		timeout = kerbDefaultTimeout
	}
	if timeout > kerbMaxTimeout {
		timeout = kerbMaxTimeout
	}

	host, port := kerbHostPort(kdc)
	if err := act.AuthorizeHost(host, port); err != nil {
		return "", err
	}

	results, err := act.Roast(ctx, kdc, realm, accounts, passwords, time.Duration(timeout)*time.Second)
	if err != nil {
		return "", err
	}

	// The validated passwords go to a file and nowhere else. A password in
	// the transcript is a password in the chat history, in the session file
	// and in whatever the transcript is later pasted into.
	outName := strings.TrimSpace(kerbStringArg(spec.Args["out"]))
	validated := 0
	for _, result := range results {
		if result.Validated {
			validated++
		}
	}
	path := ""
	if validated > 0 || outName != "" {
		written, err := kerbWriteResults(outName, results)
		if err != nil {
			return "", err
		}
		path = written
	}

	return formatKerbResults(results, kdc, realm, len(passwords), path), nil
}

// formatKerbResults renders the roast.
//
// The password itself is never here. The output says which accounts were
// checked, which one validated, and where the validated password is.
func formatKerbResults(results []act.RoastResult, kdc string, realm string, candidates int, path string) string {
	var b strings.Builder
	attempts := 0
	validated := 0
	for _, result := range results {
		if result.Error == "" {
			attempts += candidates
		}
		if result.Validated {
			validated++
		}
	}
	fmt.Fprintf(&b, "kerb: %d accounts against %d candidate passwords (%d attempts) on %s, realm %s\n",
		len(results), candidates, attempts, kdc, realm)
	for _, result := range results {
		switch {
		case result.Error != "":
			fmt.Fprintf(&b, "error: %s (%s)\n", result.Account, result.Error)
		case result.Validated:
			fmt.Fprintf(&b, "validated: %s (the password for %s is in the result file)\n", result.Account, result.Account)
		default:
			fmt.Fprintf(&b, "checked: %s (no candidate matched)\n", result.Account)
		}
	}
	fmt.Fprintf(&b, "%d validated", validated)
	if path != "" {
		fmt.Fprintf(&b, ", the result file is %s", path)
	} else {
		b.WriteString(", no result file was written")
	}
	return strings.TrimRight(b.String(), "\n")
}

// kerbWriteResults writes the validated passwords to a file under the data
// directory, at 0600.
//
// The name is a plain name: a name that could escape the directory is a
// name that could write anywhere, and the same rule the session jar keeps
// applies here.
func kerbWriteResults(name string, results []act.RoastResult) (string, error) {
	if name == "" {
		name = fmt.Sprintf("roast-%s.txt", time.Now().UTC().Format("20060102T150405Z"))
	}
	if err := kerbValidateName(name); err != nil {
		return "", err
	}
	dir := filepath.Join(paths.DataDir(), "recon", "kerb")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("the result directory could not be created: %w", err)
	}
	path := filepath.Join(dir, name)

	var b strings.Builder
	b.WriteString("# ceraa kerberos roast results\n")
	for _, result := range results {
		if !result.Validated {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", result.Account, result.Password)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return "", fmt.Errorf("the result file could not be written: %w", err)
	}
	return path, nil
}

// kerbValidateName refuses a name that is not a plain file name.
func kerbValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("the result file name is empty")
	}
	if len(name) > kerbMaxNameLength {
		return fmt.Errorf("the result file name is %d characters, over the %d bound", len(name), kerbMaxNameLength)
	}
	if strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return fmt.Errorf("the result file name %q must be a plain name, not a path", name)
	}
	return nil
}

// kerbHostPort splits a KDC address into a host and a port.
func kerbHostPort(kdc string) (string, int) {
	if host, port, err := net.SplitHostPort(strings.TrimSpace(kdc)); err == nil {
		number, err := strconv.Atoi(port)
		if err != nil {
			number = 88
		}
		return host, number
	}
	return strings.TrimSpace(kdc), 88
}

// kerbStringArg reads a string argument without the zero-value trap.
func kerbStringArg(v any) string {
	s, _ := v.(string)
	return s
}

// kerbIntArg reads an integer argument in both the native and the
// JSON-decoded shape.
func kerbIntArg(v any) (int, bool) {
	switch typed := v.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		if typed == float64(int(typed)) {
			return int(typed), true
		}
	}
	return 0, false
}

// kerbStringListArg reads a list of strings in both the native and the
// JSON-decoded shape, dropping the entries that are not strings.
func kerbStringListArg(v any) []string {
	switch typed := v.(type) {
	case []string:
		out := make([]string, 0, len(typed))
		for _, entry := range typed {
			if strings.TrimSpace(entry) != "" {
				out = append(out, entry)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(typed))
		for _, entry := range typed {
			if s, ok := entry.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
