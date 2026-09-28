// Package redact scrubs secrets from text and structured data before
// persistence, so credentials that a bot reads (via ReadFile, Glob results,
// shell output) never enter the transcript permanently.
package redact

import (
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Exact-value redaction.
//
// The patterns above are high precision by design, which is also their limit: a
// user's own secret has no vendor prefix, so a random passphrase read back by
// `env` or echoed by a provider in an error line sails straight through. A value
// the process already holds can be matched literally, with no guessing, so the
// application registers each one it stores and everything leaving the process is
// scrubbed against it as well.
//
// This is what makes it safe to hand a secret to a subprocess at all. Without it,
// a tool that injects a value into an environment is one `env` away from putting
// it in the model's context.
var (
	literalsMu sync.RWMutex
	literals   = map[string]struct{}{}
)

// MinLiteralLength is the shortest value worth matching literally.
//
// Scrubbing is substring replacement, so a short secret would corrupt unrelated
// text everywhere it appeared — a three-character secret would blank out most of
// a transcript. Below this length a value is not protectable by matching, and
// pretending otherwise would damage more than it saved.
const MinLiteralLength = 8

// RegisterLiteral adds a stored value to the exact-match scrub list.
//
// A no-op for an empty or very short value; see MinLiteralLength for why.
func RegisterLiteral(value string) {
	if len(value) < MinLiteralLength {
		return
	}
	literalsMu.Lock()
	defer literalsMu.Unlock()
	literals[value] = struct{}{}
}

// ResetLiterals clears the registered values. For tests, and for a store that
// has been reloaded.
func ResetLiterals() {
	literalsMu.Lock()
	defer literalsMu.Unlock()
	literals = map[string]struct{}{}
}

// RegisteredLiterals reports how many exact values are registered, so a caller can
// assert the scrubber is armed rather than assuming it.
func RegisteredLiterals() int {
	literalsMu.RLock()
	defer literalsMu.RUnlock()
	return len(literals)
}

// scrubLiterals replaces every registered value, longest first.
//
// Longest first because the values overlap in practice — a token and a prefix of
// it — and replacing the short one first would leave a tail of the long one
// behind, which is still most of the secret.
func scrubLiterals(text string) string {
	literalsMu.RLock()
	values := make([]string, 0, len(literals))
	for value := range literals {
		values = append(values, value)
	}
	literalsMu.RUnlock()
	if len(values) == 0 {
		return text
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	for _, value := range values {
		text = strings.ReplaceAll(text, value, mask(value))
	}
	return text
}

const redactionMarker = "«redacted"

// patterns of credential-shaped values. High precision: only matches known
// credential formats to avoid rewriting real code in transcripts.
var prefixes = []*regexp.Regexp{
	// Stripe-like, Anthropic, OpenAI
	regexp.MustCompile(`\bsk-(?:ant-|proj-|live-|test-)?[A-Za-z0-9_-]{16,}`),
	// xAI (Grok)
	regexp.MustCompile(`\bxai-[A-Za-z0-9_-]{20,}`),
	// Groq
	regexp.MustCompile(`\bgsk_[A-Za-z0-9]{40,}`),
	// HuggingFace
	regexp.MustCompile(`\bhf_[A-Za-z0-9]{30,}`),
	// GitHub tokens
	regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
	// Slack
	regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{20,}`),
	// AWS Access Key ID
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	// Google API Key
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{30,}`),
	// npm
	regexp.MustCompile(`\bnpm_[A-Za-z0-9]{20,}`),
	// JWT
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
}

var bearer = regexp.MustCompile(`(?i)(\bbearer\s+)([A-Za-z0-9._~+/=-]{12,})`)

// key=value patterns - use ReplaceAllStringFunc for manual handling since Go regexp lacks backrefs
var keyValueRe = regexp.MustCompile(
	`(?i)\b((?:[A-Za-z0-9_-]*_)?(?:api[_-]?key|apikey|secret|token|password|passwd|authorization|auth[_-]?token|access[_-]?key|private[_-]?key)s?)(["']?\s*[=:]\s*)(["']?)([A-Za-z0-9._~+/=-]{8,})["']?`,
)

// key= suffix patterns (X_KEY=value, xai-key=value)
var keySuffix = regexp.MustCompile(
	`(?i)\b([A-Za-z][A-Za-z0-9_-]*[_-]key)s?(=)(["']?)([A-Za-z0-9._~+/=-]+)`,
)

// --flag value, --flag=value flags
var secretFlagRe = regexp.MustCompile(
	`(--(?:token|password|passwd|api-key|apikey|secret|access-key|auth-token)(?:=|\s+))(["']?)([A-Za-z0-9._~+/=-]+)`,
)

// URL with password: scheme://user:secret@host
var urlUserinfo = regexp.MustCompile(
	`(\b[a-z][a-z0-9+.-]*://[^\s/:@'"]+:)([^\s/@'"«»]+)(@)`,
)

// mask produces a stable redaction marker with length info.
func mask(value string) string {
	return "«redacted " + itoa(len(value)) + " chars»"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// SecretsInText scrubs credential-shaped values and every registered exact value
// from arbitrary text.
func SecretsInText(text string) string {
	if len(text) < 8 {
		return text
	}
	// Literals first: they are exact, so they beat a pattern that would only
	// partially match, and a masked value must not be re-matched by a later pass.
	out := scrubLiterals(text)

	// Prefix formats (sk-xxx, xai-xxx, etc.)
	for _, re := range prefixes {
		out = re.ReplaceAllStringFunc(out, func(m string) string {
			if strings.HasPrefix(m, redactionMarker) {
				return m
			}
			return mask(m)
		})
	}

	// Bearer tokens
	out = bearer.ReplaceAllStringFunc(out, func(m string) string {
		if strings.HasPrefix(m, redactionMarker) {
			return m
		}
		return mask(m)
	})

	// key=value patterns
	out = keyValueRe.ReplaceAllStringFunc(out, func(m string) string {
		return mask(m)
	})

	// key= suffix patterns
	out = keySuffix.ReplaceAllStringFunc(out, func(m string) string {
		return mask(m)
	})

	// --flag value patterns
	out = secretFlagRe.ReplaceAllStringFunc(out, func(m string) string {
		return mask(m)
	})

	// URL passwords
	out = urlUserinfo.ReplaceAllStringFunc(out, func(m string) string {
		return mask(m)
	})

	return out
}

// Secrets scrubs credential values from structured data (JSON-like).
// Handles map[string]string (env vars) and slice of {name, value} objects.
func Secrets(input interface{}) interface{} {
	return redactRecursive(input, 0)
}

func redactRecursive(input interface{}, depth int) interface{} {
	if depth > 12 {
		return input
	}
	switch v := input.(type) {
	case string:
		return SecretsInText(v)
	case map[string]interface{}:
		out := make(map[string]interface{}, len(v))
		for k, val := range v {
			if isSecretName(k) {
				if s, ok := val.(string); ok {
					out[k] = mask(s)
					continue
				}
			}
			out[k] = redactRecursive(val, depth+1)
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(v))
		for k, val := range v {
			if isSecretName(k) {
				out[k] = mask(val)
			} else {
				out[k] = SecretsInText(val)
			}
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(v))
		for i, item := range v {
			// ACP env entry: {"name": "...", "value": "..."}
			if isEnvEntry(item) {
				entry := item.(map[string]interface{})
				name := entry["name"].(string)
				if isSecretName(name) {
					entry["value"] = mask(entry["value"].(string))
				} else {
					entry["value"] = SecretsInText(entry["value"].(string))
				}
				out[i] = entry
				continue
			}
			out[i] = redactRecursive(item, depth+1)
		}
		return out
	default:
		return input
	}
}

func isSecretName(name string) bool {
	lower := strings.ToLower(name)
	secretParts := []string{"token", "secret", "password", "passwd", "apikey", "api_key", "authorization", "auth_token"}
	for _, part := range secretParts {
		if strings.Contains(lower, part) {
			return true
		}
	}
	// "key" as standalone or suffix, but not "public_key"
	if lower == "key" {
		return true
	}
	if strings.HasSuffix(lower, "_key") || strings.HasSuffix(lower, "-key") || strings.HasSuffix(lower, ".key") {
		// Allow public_key / public-key (not sensitive)
		if strings.Contains(lower, "public") {
			return false
		}
		return true
	}
	return false
}

func isEnvEntry(item interface{}) bool {
	m, ok := item.(map[string]interface{})
	if !ok {
		return false
	}
	_, hasName := m["name"]
	_, hasValue := m["value"]
	return hasName && hasValue
}
