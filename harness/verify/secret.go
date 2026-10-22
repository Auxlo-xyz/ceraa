package verify

// secret.go is the credential-shaped value list, and the masking of what it
// finds.
//
// The list mirrors harness/redact deliberately. The redactor already knows
// what a credential looks like because it exists to remove them, and a
// scanner that disagreed with the redactor about that would be a bug waiting
// to happen. harness/redact does not export its patterns, so they are
// restated here identically and the agreement is pinned by a test that runs
// both lists over the same corpus.
//
// The masking is the redactor's form, character for character, so a finding
// that quotes a masked credential looks the same in both places. A finding
// that quotes a credential in full is the one place Ceraa's own redaction
// guarantee would be defeated, so the value never leaves this package in the
// clear.

import (
	"regexp"
	"strconv"
	"strings"
)

// SecretPattern is one credential shape.
type SecretPattern struct {
	// Name identifies the shape in an obligation detail, so a reader can
	// disagree with the shape without having to read the expression.
	Name string
	// RE matches the shape.
	RE *regexp.Regexp
	// SecretGroup is the capture group holding the credential itself. Zero
	// is the whole match, which is the credential for the shapes that carry
	// no context around it.
	SecretGroup int
}

// DefaultSecretPatterns is the credential-shaped value list, mirroring
// harness/redact.
//
// It is a table and not state: the policy reads it and never writes it. The
// expressions are the redactor's, character for character, so the two cannot
// drift apart without the mirror test failing.
var DefaultSecretPatterns = []SecretPattern{
	{Name: "provider api key", RE: regexp.MustCompile(`\bsk-(?:ant-|proj-|live-|test-)?[A-Za-z0-9_-]{16,}`)},
	{Name: "xai api key", RE: regexp.MustCompile(`\bxai-[A-Za-z0-9_-]{20,}`)},
	{Name: "groq api key", RE: regexp.MustCompile(`\bgsk_[A-Za-z0-9]{40,}`)},
	{Name: "huggingface token", RE: regexp.MustCompile(`\bhf_[A-Za-z0-9]{30,}`)},
	{Name: "github token", RE: regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}`)},
	{Name: "github fine grained token", RE: regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`)},
	{Name: "slack token", RE: regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{20,}`)},
	{Name: "aws access key id", RE: regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{Name: "google api key", RE: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{30,}`)},
	{Name: "npm token", RE: regexp.MustCompile(`\bnpm_[A-Za-z0-9]{20,}`)},
	{Name: "json web token", RE: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)},
	{Name: "bearer token", RE: regexp.MustCompile(`(?i)(\bbearer\s+)([A-Za-z0-9._~+/=-]{12,})`), SecretGroup: 2},
	{
		Name:        "credential in a key value pair",
		RE:          regexp.MustCompile(`(?i)\b((?:[A-Za-z0-9_-]*_)?(?:api[_-]?key|apikey|secret|token|password|passwd|authorization|auth[_-]?token|access[_-]?key|private[_-]?key)s?)(["']?\s*[=:]\s*)(["']?)([A-Za-z0-9._~+/=-]{8,})["']?`),
		SecretGroup: 4,
	},
	{
		Name:        "credential in a key suffix pair",
		RE:          regexp.MustCompile(`(?i)\b([A-Za-z][A-Za-z0-9_-]*[_-]key)s?(=)(["']?)([A-Za-z0-9._~+/=-]+)`),
		SecretGroup: 4,
	},
	{
		Name:        "credential in a command flag",
		RE:          regexp.MustCompile(`(--(?:token|password|passwd|api-key|apikey|secret|access-key|auth-token)(?:=|\s+))(["']?)([A-Za-z0-9._~+/=-]+)`),
		SecretGroup: 3,
	},
	{
		Name:        "credential in a url",
		RE:          regexp.MustCompile(`(\b[a-z][a-z0-9+.-]*://[^\s/:@'"]+:)([^\s/@'"«»]+)(@)`),
		SecretGroup: 2,
	},
}

// SecretMatch is one credential-shaped value found in a response.
type SecretMatch struct {
	// Pattern is the name of the shape that matched.
	Pattern string
	// Masked is the credential as it may be shown: its length and nothing
	// else. The value itself is never carried out of this package.
	Masked string
}

// FindSecret scans text for a credential-shaped value and returns the first
// match in pattern order.
//
// The order is the list order, so the answer never depends on which shape
// happens to be examined first.
func FindSecret(text string) (SecretMatch, bool) {
	for _, pattern := range DefaultSecretPatterns {
		if match, ok := findSecretIn(pattern, text); ok {
			return match, true
		}
	}
	return SecretMatch{}, false
}

// findSecretIn looks for one shape in a text.
func findSecretIn(pattern SecretPattern, text string) (SecretMatch, bool) {
	indexes := pattern.RE.FindAllStringSubmatchIndex(text, -1)
	if len(indexes) == 0 {
		return SecretMatch{}, false
	}
	start, end := secretSpan(pattern, indexes[0])
	if start < 0 {
		return SecretMatch{}, false
	}
	return SecretMatch{
		Pattern: pattern.Name,
		Masked:  MaskValue(text[start:end]),
	}, true
}

// secretSpan returns the span of the credential inside a match.
//
// The span is the capture group the pattern named, or the whole match when
// the pattern named none or when the group did not take part in the match.
func secretSpan(pattern SecretPattern, match []int) (int, int) {
	group := pattern.SecretGroup
	if group < 0 || 2*group+1 >= len(match) {
		group = 0
	}
	start, end := match[2*group], match[2*group+1]
	if start < 0 {
		return match[0], match[1]
	}
	return start, end
}

// MaskValue renders a credential as it may be shown: its length and nothing
// else.
//
// The form is the redactor's, character for character, so a masked value
// looks the same wherever it is read.
func MaskValue(value string) string {
	return "«redacted " + strconv.Itoa(len(value)) + " chars»"
}

// MaskSecrets masks every credential-shaped value in a text.
//
// The context around a value is kept: "api_key=«redacted 16 chars»" tells a
// reader that a credential was found and where, which "«redacted 25 chars»"
// does not.
func MaskSecrets(text string) string {
	if text == "" {
		return text
	}
	out := text
	for _, pattern := range DefaultSecretPatterns {
		out = maskInPattern(pattern, out)
	}
	return out
}

// maskInPattern replaces the credential in every match of one shape.
func maskInPattern(pattern SecretPattern, text string) string {
	indexes := pattern.RE.FindAllStringSubmatchIndex(text, -1)
	if len(indexes) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, match := range indexes {
		start, end := secretSpan(pattern, match)
		if start < 0 || start < last {
			continue
		}
		b.WriteString(text[last:start])
		b.WriteString(MaskValue(text[start:end]))
		last = end
	}
	b.WriteString(text[last:])
	return b.String()
}
