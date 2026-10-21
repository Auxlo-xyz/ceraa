package jar

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newStore opens a store in a temp dir. Every test in this package works on
// its own directory, because a session is a file on disk and a shared one
// would make the tests order-dependent.
func newStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return store
}

func sampleSession() Session {
	return Session{
		Name: "admin",
		Cookies: []Cookie{
			{Name: "sessionid", Value: "s3cr3t", Domain: "example.com", Path: "/"},
			{Name: "csrf", Value: "t0ken", Domain: "example.com", Path: "/app"},
		},
		Headers: map[string]string{"Authorization": "Bearer abc123"},
		Meta:    map[string]string{"login_url": "https://example.com/login"},
	}
}

func TestASessionSurvivesTheRoundTrip(t *testing.T) {
	store := newStore(t)
	want := sampleSession()
	if err := store.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Load("admin")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Name != want.Name {
		t.Errorf("name = %q, want %q", got.Name, want.Name)
	}
	if len(got.Cookies) != len(want.Cookies) {
		t.Fatalf("cookies = %d, want %d", len(got.Cookies), len(want.Cookies))
	}
	for i, c := range got.Cookies {
		if c != want.Cookies[i] {
			t.Errorf("cookie %d = %+v, want %+v", i, c, want.Cookies[i])
		}
	}
	if got.Headers["Authorization"] != "Bearer abc123" {
		t.Errorf("headers = %v", got.Headers)
	}
	if got.Meta["login_url"] != "https://example.com/login" {
		t.Errorf("meta = %v", got.Meta)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("timestamps = %v/%v, want both set", got.CreatedAt, got.UpdatedAt)
	}
}

// A re-login must not reset the age: the creation time is what the operator
// reads, and a session that has been in use for a week does not become new
// because it was refreshed.
func TestAReloginKeepsTheCreationTime(t *testing.T) {
	store := newStore(t)
	first := sampleSession()
	if err := store.Save(first); err != nil {
		t.Fatalf("Save: %v", err)
	}
	stored, err := store.Load("admin")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	second := sampleSession()
	second.CreatedAt = time.Time{}
	if err := store.Save(second); err != nil {
		t.Fatalf("Save again: %v", err)
	}
	again, err := store.Load("admin")
	if err != nil {
		t.Fatalf("Load again: %v", err)
	}
	if !again.CreatedAt.Equal(stored.CreatedAt) {
		t.Errorf("created = %v, want %v", again.CreatedAt, stored.CreatedAt)
	}
	if !again.UpdatedAt.After(stored.UpdatedAt) && !again.UpdatedAt.Equal(stored.UpdatedAt) {
		t.Errorf("updated = %v, want at or after %v", again.UpdatedAt, stored.UpdatedAt)
	}
}

// The listing is a catalogue. A cookie value in it would put a credential in
// every prompt that names a session.
func TestTheListingNeverCarriesACookieValue(t *testing.T) {
	store := newStore(t)
	if err := store.Save(sampleSession()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	sessions, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	got := sessions[0]
	if got.Name != "admin" {
		t.Errorf("name = %q", got.Name)
	}
	if len(got.Cookies) != 2 {
		t.Fatalf("cookies = %d, want 2", len(got.Cookies))
	}
	for _, c := range got.Cookies {
		if c.Value != "" {
			t.Errorf("cookie %q carries the value %q", c.Name, c.Value)
		}
	}
	if got.Cookies[0].Name != "sessionid" || got.Cookies[1].Name != "csrf" {
		t.Errorf("cookie names = %v", got.Cookies)
	}
	if got.Meta["login_url"] != "https://example.com/login" {
		t.Errorf("meta = %v", got.Meta)
	}
}

func TestSessionNamesAreValidated(t *testing.T) {
	store := newStore(t)
	refused := []string{
		"",
		"..",
		"../escape",
		"..\\escape",
		"nested/name",
		"nested\\name",
		".hidden",
		"has space",
		"has/slash",
		strings.Repeat("a", maxNameLen+1),
	}
	for _, name := range refused {
		if err := store.Save(Session{Name: name}); err == nil {
			t.Errorf("Save accepted the name %q", name)
		}
		if _, err := store.Load(name); err == nil {
			t.Errorf("Load accepted the name %q", name)
		}
		if err := store.Delete(name); err == nil {
			t.Errorf("Delete accepted the name %q", name)
		}
	}
	accepted := []string{"admin", "app.test", "user_1", "a-b-c", "UPPER"}
	for _, name := range accepted {
		if err := store.Save(Session{Name: name}); err != nil {
			t.Errorf("Save refused the name %q: %v", name, err)
		}
	}
}

// A session file holds credentials, so it is written 0600 and stays 0600
// across a rewrite.
func TestTheSessionFileIsPrivate(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.Save(sampleSession()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "admin.json"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

func TestDeleteRemovesTheSession(t *testing.T) {
	store := newStore(t)
	if err := store.Save(sampleSession()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Delete("admin"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Load("admin"); err == nil {
		t.Error("the session survived the delete")
	}
	if err := store.Delete("admin"); err == nil {
		t.Error("deleting a session that is not there reported success")
	}
}

// The cookie rules are the browser's, because a replay that sends a cookie
// the browser would refuse is a test of a session nobody holds.
func TestTheCookieHeaderFollowsTheBrowserRules(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)

	cases := []struct {
		name string
		sess Session
		url  string
		want string
	}{
		{
			name: "an exact host matches",
			sess: Session{Cookies: []Cookie{{Name: "a", Value: "1", Domain: "example.com", Path: "/"}}},
			url:  "https://example.com/x",
			want: "a=1",
		},
		{
			name: "a subdomain is covered by the domain",
			sess: Session{Cookies: []Cookie{{Name: "a", Value: "1", Domain: "example.com", Path: "/"}}},
			url:  "https://api.example.com/x",
			want: "a=1",
		},
		{
			name: "a leading dot is the domain wide spelling",
			sess: Session{Cookies: []Cookie{{Name: "a", Value: "1", Domain: ".example.com", Path: "/"}}},
			url:  "https://api.example.com/x",
			want: "a=1",
		},
		{
			name: "the reverse is not a match",
			sess: Session{Cookies: []Cookie{{Name: "a", Value: "1", Domain: "api.example.com", Path: "/"}}},
			url:  "https://example.com/x",
			want: "",
		},
		{
			name: "a foreign domain is not a match",
			sess: Session{Cookies: []Cookie{{Name: "a", Value: "1", Domain: "other.com", Path: "/"}}},
			url:  "https://example.com/x",
			want: "",
		},
		{
			name: "a path prefix matches",
			sess: Session{Cookies: []Cookie{{Name: "a", Value: "1", Domain: "example.com", Path: "/app"}}},
			url:  "https://example.com/app/panel",
			want: "a=1",
		},
		{
			name: "a path prefix stops at the boundary",
			sess: Session{Cookies: []Cookie{{Name: "a", Value: "1", Domain: "example.com", Path: "/app"}}},
			url:  "https://example.com/apple",
			want: "",
		},
		{
			name: "a secure cookie needs https",
			sess: Session{Cookies: []Cookie{{Name: "a", Value: "1", Domain: "example.com", Path: "/", Secure: true}}},
			url:  "http://example.com/x",
			want: "",
		},
		{
			name: "a secure cookie rides https",
			sess: Session{Cookies: []Cookie{{Name: "a", Value: "1", Domain: "example.com", Path: "/", Secure: true}}},
			url:  "https://example.com/x",
			want: "a=1",
		},
		{
			name: "an expired cookie is not sent",
			sess: Session{Cookies: []Cookie{{Name: "a", Value: "1", Domain: "example.com", Path: "/", Expires: past}}},
			url:  "https://example.com/x",
			want: "",
		},
		{
			name: "a live cookie is sent",
			sess: Session{Cookies: []Cookie{{Name: "a", Value: "1", Domain: "example.com", Path: "/", Expires: future}}},
			url:  "https://example.com/x",
			want: "a=1",
		},
		{
			name: "only the applicable cookies are sent",
			sess: Session{Cookies: []Cookie{
				{Name: "wide", Value: "1", Domain: "example.com", Path: "/"},
				{Name: "scoped", Value: "2", Domain: "example.com", Path: "/app"},
				{Name: "secure", Value: "3", Domain: "example.com", Path: "/", Secure: true},
			}},
			url:  "http://example.com/app/panel",
			want: "wide=1; scoped=2",
		},
		{
			name: "no cookies means no header",
			sess: Session{},
			url:  "https://example.com/x",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := url.Parse(tc.url)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.url, err)
			}
			if got := CookieHeader(tc.sess, parsed); got != tc.want {
				t.Errorf("CookieHeader = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTheCookieHeaderOfNoURLIsEmpty(t *testing.T) {
	if got := CookieHeader(sampleSession(), nil); got != "" {
		t.Errorf("CookieHeader(nil) = %q, want empty", got)
	}
}
