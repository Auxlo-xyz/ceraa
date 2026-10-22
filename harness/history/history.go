// Package history reads the receipt trail back and replays what it recorded.
//
// The receipts are the evidence a probe leaves behind. This package turns
// them into something a session can query: what did I send to this host,
// what came back, and what happens if I send it again with one thing
// changed. That last one is the primitive every authentication and access
// control test needs, and it only exists when the request is recorded.
package history

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ceraa/harness/recon"
)

// Entry is one recorded request.
type Entry struct {
	// ID is the receipt file name without the extension.
	ID string `json:"id"`
	// At is when the request went out.
	At time.Time `json:"at"`
	// Method and URL are the request.
	Method string `json:"method"`
	URL    string `json:"url"`
	// Status and SHA256 are the response.
	Status int    `json:"status"`
	SHA256 string `json:"sha256"`
	// Headers are the response headers the receipt kept.
	Headers map[string]string `json:"headers,omitempty"`
	// BodyHead is the bounded body the receipt kept.
	BodyHead string `json:"body_head,omitempty"`
	// RedirectedTo is where a 3xx pointed, when it did.
	RedirectedTo string `json:"redirected_to,omitempty"`
	// Path is the receipt file on disk.
	Path string `json:"path,omitempty"`
}

// Filter narrows a query.
type Filter struct {
	// Host matches the URL's host, exactly or as a suffix.
	Host string
	// PathContains matches anywhere in the URL.
	PathContains string
	// Method matches the request method.
	Method string
	// Status matches the response status.
	Status int
	// Since and Until bound the time.
	Since time.Time
	Until time.Time
	// Limit caps the entries returned, newest first.
	Limit int
}

// receiptDoc mirrors the receipt the probe writes.
type receiptDoc struct {
	At           string            `json:"at"`
	URL          string            `json:"url"`
	Method       string            `json:"method"`
	Status       int               `json:"status"`
	SHA256       string            `json:"sha256"`
	Headers      map[string]string `json:"headers"`
	BodyHead     string            `json:"body_head"`
	RedirectedTo string            `json:"redirected_to,omitempty"`
}

// Query reads the receipts under a receipts directory and returns the
// entries that match.
//
// The directory is the one the probe writes to, so the history is the trail
// itself and not a second copy of it.
func Query(receiptsDir string, filter Filter) ([]Entry, error) {
	entries, err := readAll(receiptsDir)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		if !matches(entry, filter) {
			continue
		}
		out = append(out, entry)
	}
	// Newest first, because the question is usually about what just
	// happened.
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

// Hosts returns the distinct hosts in the trail.
func Hosts(receiptsDir string) ([]string, error) {
	entries, err := readAll(receiptsDir)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := []string{}
	for _, entry := range entries {
		host := hostOf(entry.URL)
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		out = append(out, host)
	}
	sort.Strings(out)
	return out, nil
}

// Change is what a replay alters.
type Change struct {
	// Method replaces the method.
	Method string
	// URL replaces the url.
	URL string
	// Body replaces the body.
	Body string
	// Headers are added to the request.
	Headers map[string]string
	// RemoveHeaders are dropped from the request.
	RemoveHeaders []string
	// Query sets query parameters, replacing any with the same name.
	Query map[string]string
}

// Replay sends the recorded request again with the change applied.
//
// The receipt keeps the response's headers and the request's method and url.
// It does not keep the request's headers, because a receipt that carried
// credentials would be a credential store. So a replay sends what was
// recorded and what the caller adds, and says so.
func Replay(ctx context.Context, entry Entry, change Change) (recon.Result, error) {
	method := change.Method
	if method == "" {
		method = entry.Method
	}
	if method == "" {
		method = "GET"
	}
	url := change.URL
	if url == "" {
		url = entry.URL
	}
	if len(change.Query) > 0 {
		updated, err := applyQuery(url, change.Query)
		if err != nil {
			return recon.Result{}, err
		}
		url = updated
	}
	headers := map[string]string{}
	for name, value := range change.Headers {
		headers[name] = value
	}
	for _, name := range change.RemoveHeaders {
		delete(headers, name)
	}
	if err := recon.Gate(url); err != nil {
		return recon.Result{}, err
	}
	if err := recon.Take(1); err != nil {
		return recon.Result{}, err
	}
	return recon.Probe(ctx, recon.Request{
		URL:     url,
		Method:  method,
		Headers: headers,
		Body:    change.Body,
		Timeout: 15,
	})
}

// applyQuery sets query parameters on a url, replacing the ones with the
// same name.
func applyQuery(rawurl string, values map[string]string) (string, error) {
	parsed, err := url.Parse(rawurl)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	for name, value := range values {
		query.Set(name, value)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// readAll reads every receipt in a directory.
func readAll(dir string) ([]Entry, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("the receipts at %s could not be read: %w", dir, err)
	}
	out := make([]Entry, 0, len(items))
	for _, item := range items {
		if item.IsDir() || !strings.HasSuffix(item.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, item.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var doc receiptDoc
		if err := json.Unmarshal(data, &doc); err != nil {
			continue
		}
		at, _ := time.Parse(time.RFC3339Nano, doc.At)
		out = append(out, Entry{
			ID:           strings.TrimSuffix(item.Name(), ".json"),
			At:           at,
			Method:       doc.Method,
			URL:          doc.URL,
			Status:       doc.Status,
			SHA256:       doc.SHA256,
			Headers:      doc.Headers,
			BodyHead:     doc.BodyHead,
			RedirectedTo: doc.RedirectedTo,
			Path:         path,
		})
	}
	return out, nil
}

// matches reports whether an entry satisfies a filter.
func matches(entry Entry, filter Filter) bool {
	if filter.Host != "" {
		host := hostOf(entry.URL)
		if host != filter.Host && !strings.HasSuffix(host, "."+filter.Host) {
			return false
		}
	}
	if filter.PathContains != "" && !strings.Contains(entry.URL, filter.PathContains) {
		return false
	}
	if filter.Method != "" && !strings.EqualFold(entry.Method, filter.Method) {
		return false
	}
	if filter.Status != 0 && entry.Status != filter.Status {
		return false
	}
	if !filter.Since.IsZero() && entry.At.Before(filter.Since) {
		return false
	}
	if !filter.Until.IsZero() && entry.At.After(filter.Until) {
		return false
	}
	return true
}

// hostOf reads the host out of a url.
func hostOf(rawurl string) string {
	parsed, err := url.Parse(rawurl)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}
