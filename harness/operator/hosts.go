// Package operator reads what the operator actually said.
//
// The harness needs one thing it cannot get from the model: a signal that
// comes from the human rather than from the agent. The recon layer uses it
// to tell a target the operator asked about from a target the model
// invented, and it is derived from the transcript rather than from a form,
// because a form between saying what to do and doing it is a chain the
// operator did not ask for.
package operator

import (
	"regexp"
	"strings"
)

// domainToken matches a host-like token: dotted labels ending in a letter,
// which covers every real domain and excludes file names with extensions.
var domainToken = regexp.MustCompile(`(?i)\b(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.)+[a-z][a-z0-9-]*\b`)

// addressToken matches a literal IPv4 address.
var addressToken = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)

// singleLabel matches a bare host such as "localhost" or a container name.
var singleLabel = regexp.MustCompile(`(?i)\blocalhost\b`)

// fileExtensions are the last labels that are a file extension rather than a
// domain. A transcript is full of paths, and "index.html" is a filename, not
// a target: reading it as a host would let a filename authorize a request.
var fileExtensions = map[string]bool{
	"html": true, "htm": true, "js": true, "mjs": true, "cjs": true,
	"ts": true, "tsx": true, "jsx": true, "py": true, "rb": true, "go": true,
	"rs": true, "java": true, "php": true, "c": true, "h": true, "cpp": true,
	"css": true, "json": true, "yaml": true, "yml": true, "toml": true,
	"ini": true, "cfg": true, "conf": true, "env": true, "md": true,
	"txt": true, "csv": true, "xml": true, "svg": true, "png": true,
	"jpg": true, "jpeg": true, "gif": true, "webp": true, "ico": true,
	"pdf": true, "zip": true, "tar": true, "gz": true, "lock": true,
	"log": true, "sh": true, "sql": true, "map": true, "min": true,
}

// Hosts returns the distinct host-like tokens in the text, lowercased and
// without ports, in the order they first appeared.
//
// The result is what the recon layer compares a destination against, so it
// is deliberately forgiving about form (a path, a URL, a bare name) and
// strict about identity (a token is a host, not a substring of a sentence).
func Hosts(text string) []string {
	if text == "" {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	add := func(token string) {
		token = strings.ToLower(strings.Trim(token, "."))
		if token == "" || seen[token] {
			return
		}
		if labels := strings.Split(token, "."); len(labels) > 1 && fileExtensions[labels[len(labels)-1]] {
			return
		}
		seen[token] = true
		out = append(out, token)
	}
	for _, match := range domainToken.FindAllString(text, -1) {
		add(match)
	}
	for _, match := range addressToken.FindAllString(text, -1) {
		add(match)
	}
	for _, match := range singleLabel.FindAllString(text, -1) {
		add(match)
	}
	return out
}

// Join renders a host list for transport, and reads it back.
func Join(hosts []string) string { return strings.Join(hosts, ",") }

// Split parses a joined host list.
func Split(list string) []string {
	if strings.TrimSpace(list) == "" {
		return nil
	}
	parts := strings.Split(list, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.ToLower(strings.TrimSpace(part))
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// Covers reports whether a named host covers the destination.
//
// A name covers itself and its subdomains, because an operator who names
// example.com has named api.example.com as much as anyone who names it
// explicitly. The reverse is not true: naming one subdomain does not
// authorize the parent.
func Covers(named []string, host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	for _, candidate := range named {
		candidate = strings.ToLower(strings.TrimSpace(candidate))
		if candidate == "" {
			continue
		}
		if host == candidate || strings.HasSuffix(host, "."+candidate) {
			return true
		}
	}
	return false
}
