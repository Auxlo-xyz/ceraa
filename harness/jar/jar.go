// Package jar keeps authenticated sessions on disk.
//
// The recon layer could probe a URL but could not log in and keep the result,
// so every authenticated test - IDOR, BOLA, an admin panel, CSRF - had to be
// done by hand. This package is the session jar: named logins under the data
// directory, with the cookies kept, because a session without its cookies is
// not a session.
//
// Three rules shape it:
//
//   - A cookie value is a credential. It lives in a 0600 file, and the listing
//     shows cookie names, never values.
//   - A session name is a filename. It is validated, because a name that could
//     escape the session directory is a name that could write anywhere.
//   - A write is atomic: a temp file plus a rename, so a crash mid-write
//     cannot leave a half-written session that a later read mistakes for a
//     good one.
package jar

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// maxNameLen bounds a session name. The name is a filename and it travels
// through a model, so it is short by construction rather than by hope.
const maxNameLen = 64

// Cookie is one stored cookie. Values are stored because a session without
// them is not a session.
type Cookie struct {
	Name     string    `json:"name"`
	Value    string    `json:"value"`
	Domain   string    `json:"domain"`
	Path     string    `json:"path"`
	Expires  time.Time `json:"expires"`
	Secure   bool      `json:"secure"`
	HTTPOnly bool      `json:"http_only"`
}

// Session is one named login.
type Session struct {
	Name    string   `json:"name"`
	Cookies []Cookie `json:"cookies"`
	// Headers are the request headers worth replaying, an Authorization
	// header above all: the target issued it once and will not issue it
	// again.
	Headers map[string]string `json:"headers,omitempty"`
	// Meta is what the operator or the model noted about the session.
	Meta      map[string]string `json:"meta,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// Store keeps sessions under a directory, one file per session.
type Store struct {
	dir string
}

// Open prepares a session directory. The directory is created if it is not
// there, because a first login is the first thing that ever needs it.
func Open(dir string) (*Store, error) {
	trimmed := strings.TrimSpace(dir)
	if trimmed == "" {
		return nil, errors.New("jar: a session directory is required")
	}
	if err := os.MkdirAll(trimmed, 0o700); err != nil {
		return nil, fmt.Errorf("jar: create the session directory: %w", err)
	}
	return &Store{dir: trimmed}, nil
}

// Dir is the directory the sessions live in.
func (s *Store) Dir() string { return s.dir }

// path is the file one session lives in.
func (s *Store) path(name string) string {
	return filepath.Join(s.dir, name+".json")
}

// Save writes a session, replacing any session of the same name.
//
// The write is atomic: a temp file in the same directory, then a rename. A
// reader either sees the old session or the new one, never a half-written
// one, and the rename does not care that the temp file has a different name.
func (s *Store) Save(sess Session) error {
	if err := ValidateName(sess.Name); err != nil {
		return err
	}
	now := time.Now().UTC()
	if existing, err := s.load(sess.Name); err == nil && !existing.CreatedAt.IsZero() {
		// A re-login updates the session rather than replacing it: the
		// creation time is the age the operator reads, and resetting it on
		// every login would make a week-old session look new.
		sess.CreatedAt = existing.CreatedAt
	}
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = now
	}
	sess.UpdatedAt = now

	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return fmt.Errorf("jar: encode session %q: %w", sess.Name, err)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("jar: create the session directory: %w", err)
	}
	// The temp name is unique, not a fixed one: two logins to the same
	// session in one batch would otherwise write over each other's temp
	// file and rename a spliced document into place.
	tmp, err := os.CreateTemp(s.dir, ".session-*.tmp")
	if err != nil {
		return fmt.Errorf("jar: write session %q: %w", sess.Name, err)
	}
	tmpName := tmp.Name()
	// A successful rename makes this a no-op; a failed one cleans up.
	defer os.Remove(tmpName)
	// CreateTemp already opens the file 0600, and a session file holds
	// credentials.
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("jar: write session %q: %w", sess.Name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("jar: write session %q: %w", sess.Name, err)
	}
	if err := os.Rename(tmpName, s.path(sess.Name)); err != nil {
		return fmt.Errorf("jar: store session %q: %w", sess.Name, err)
	}
	return nil
}

// Load reads one session, values included.
func (s *Store) Load(name string) (Session, error) {
	if err := ValidateName(name); err != nil {
		return Session{}, err
	}
	return s.load(name)
}

// load reads a session without re-validating the name. The listing calls it
// with names it read off the disk, and a file this store did not write is not
// a session it has to police.
func (s *Store) load(name string) (Session, error) {
	data, err := os.ReadFile(s.path(name))
	if err != nil {
		return Session{}, fmt.Errorf("jar: no session named %q: %w", name, err)
	}
	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return Session{}, fmt.Errorf("jar: session %q is not readable: %w", name, err)
	}
	// The file name is the truth about what a session is called: a hand
	// edited file cannot rename itself into another session's slot.
	sess.Name = name
	return sess, nil
}

// List returns the stored sessions as a catalogue: names, ages, cookie names
// and notes, never cookie values.
//
// The values are stripped rather than the cookies dropped, because the cookie
// names are what a reader needs to know which session holds what.
func (s *Store) List() ([]Session, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Session{}, nil
		}
		return nil, fmt.Errorf("jar: read the session directory: %w", err)
	}
	out := make([]Session, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		file := entry.Name()
		if !strings.HasSuffix(file, ".json") {
			continue
		}
		sess, err := s.load(strings.TrimSuffix(file, ".json"))
		if err != nil {
			// A file that is not a session is not a session, and one bad
			// file must not hide the good ones.
			continue
		}
		redacted := make([]Cookie, 0, len(sess.Cookies))
		for _, c := range sess.Cookies {
			c.Value = ""
			redacted = append(redacted, c)
		}
		sess.Cookies = redacted
		out = append(out, sess)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Delete removes a session file. A missing session is an error, so the caller
// can say which name was not there.
func (s *Store) Delete(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if err := os.Remove(s.path(name)); err != nil {
		return fmt.Errorf("jar: delete session %q: %w", name, err)
	}
	return nil
}

// ValidateName refuses a session name that could escape the session directory
// or that the listing could not describe.
//
// The name is a filename the model chose, and a filename is a path. The
// character set is deliberately narrow: a name a person can type twice the
// same way is a name a person can ask for twice the same way.
func ValidateName(name string) error {
	if name == "" {
		return errors.New("jar: a session name is required")
	}
	if len(name) > maxNameLen {
		return fmt.Errorf("jar: session name is longer than %d characters", maxNameLen)
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("jar: session name %q must not contain a path separator", name)
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("jar: session name %q must not start with a dot", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("jar: session name %q may contain only letters, digits, dash, underscore and dot", name)
		}
	}
	return nil
}

// CookieHeader renders the Cookie header value for a URL.
//
// The rules are the ones a browser applies, because a replay that sends a
// cookie the browser would not send is a test of a session the operator does
// not have: the domain must cover the host (and never the other way round),
// the path must cover the request path, a Secure cookie goes over https
// only, and an expired cookie is not sent at all.
//
// An empty string means nothing applies, which is a valid answer: a login
// that set no cookies produces no Cookie header.
func CookieHeader(sess Session, u *url.URL) string {
	if u == nil {
		return ""
	}
	host := strings.ToLower(strings.Trim(u.Hostname(), "[]"))
	if host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	path := u.Path
	if path == "" {
		path = "/"
	}
	now := time.Now()
	parts := make([]string, 0, len(sess.Cookies))
	for _, c := range sess.Cookies {
		if c.Name == "" {
			continue
		}
		if !c.Expires.IsZero() && !c.Expires.After(now) {
			continue
		}
		if c.Secure && scheme != "https" {
			continue
		}
		if !domainMatch(c.Domain, host) {
			continue
		}
		if !pathMatch(c.Path, path) {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// domainMatch reports whether a cookie's domain covers a host.
//
// The direction is the point: a cookie set for api.example.com belongs to
// that host and to its subdomains, and never to example.com, which is the
// parent a subdomain's cookie must not escape into. A leading dot is the
// domain-wide spelling and carries the same rule, so it is stripped rather
// than compared as a character.
func domainMatch(domain, host string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	d = strings.Trim(d, ".")
	if d == "" {
		return false
	}
	if host == d {
		return true
	}
	return strings.HasSuffix(host, "."+d)
}

// pathMatch applies the prefix rule with the boundary a browser uses: a
// cookie for /admin covers /admin and /admin/panel, but not /administrator.
func pathMatch(cookiePath, requestPath string) bool {
	if cookiePath == "" {
		cookiePath = "/"
	}
	if requestPath == "" {
		requestPath = "/"
	}
	if cookiePath == requestPath {
		return true
	}
	if !strings.HasPrefix(requestPath, cookiePath) {
		return false
	}
	if strings.HasSuffix(cookiePath, "/") {
		return true
	}
	return requestPath[len(cookiePath)] == '/'
}
