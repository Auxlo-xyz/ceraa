package creds

import (
	"context"
	"strings"
	"sync"
	"testing"

	"ceraa/harness/jar"
	"ceraa/harness/recon"
)

// The spray is exercised through a fake probe, so the suite never reaches
// the network and never charges an operator's budget. The gate still runs,
// because the gate is part of the contract: a refused attempt must not be
// sent.

// fakeProbe answers from a script and records what it was asked.
type fakeProbe struct {
	mu       sync.Mutex
	requests []recon.Request
	answer   func(req recon.Request) (Attempt, error)
}

func (f *fakeProbe) probe(ctx context.Context, req recon.Request, bodyCap int) (Attempt, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.answer == nil {
		return attemptFor(401, `{"error":"invalid credentials"}`), nil
	}
	return f.answer(req)
}

func (f *fakeProbe) bodies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.requests))
	for _, req := range f.requests {
		out = append(out, req.Body)
	}
	return out
}

func (f *fakeProbe) urls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.requests))
	for _, req := range f.requests {
		out = append(out, req.URL)
	}
	return out
}

// attemptFor builds one observation.
func attemptFor(status int, body string) Attempt {
	return Attempt{
		Result: recon.Result{
			Status:    status,
			SHA256:    "0123456789abcdef0123456789abcdef",
			ElapsedMS: 7,
			ReceiptID: "20260101T000000.000000000Z-0123456789ab.json",
		},
		Body: []byte(body),
	}
}

// hermetic points the data directory at a scratch one, so a scope record
// left on the machine cannot change what the gate decides.
func hermetic(t *testing.T) {
	t.Helper()
	t.Setenv("CERAA_HOME", t.TempDir())
}

// TestTheSprayRendersTheBodyTemplate: a JSON login and a form login both
// carry the credential, because the template is what says where.
func TestTheSprayRendersTheBodyTemplate(t *testing.T) {
	hermetic(t)
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "json",
			body: `{"email":"{USERNAME}","password":"{PASSWORD}"}`,
			want: `{"email":"admin","password":"s3cret"}`,
		},
		{
			name: "form",
			body: "username={USERNAME}&password={PASSWORD}",
			want: "username=admin&password=s3cret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeProbe{}
			if _, err := Spray(context.Background(), SprayOptions{
				URL:         "https://example.com/login",
				Body:        tc.body,
				Credentials: []Credential{{Username: "admin", Password: "s3cret"}},
				Probe:       fake.probe,
			}); err != nil {
				t.Fatalf("Spray: %v", err)
			}
			bodies := fake.bodies()
			if len(bodies) != 1 {
				t.Fatalf("requests = %d, want 1", len(bodies))
			}
			if bodies[0] != tc.want {
				t.Errorf("body = %q, want %q", bodies[0], tc.want)
			}
		})
	}
}

// TestTheSprayRendersTheFieldNamePlaceholders: the caller's field names are
// placeholders too, so a login that calls the field "email" works without
// the engine having to know it.
func TestTheSprayRendersTheFieldNamePlaceholders(t *testing.T) {
	hermetic(t)
	fake := &fakeProbe{}
	if _, err := Spray(context.Background(), SprayOptions{
		URL:           "https://example.com/login",
		Body:          `{"email":"{email}","pass":"{pass}"}`,
		UsernameField: "email",
		PasswordField: "pass",
		Credentials:   []Credential{{Username: "admin", Password: "s3cret"}},
		Probe:         fake.probe,
	}); err != nil {
		t.Fatalf("Spray: %v", err)
	}
	bodies := fake.bodies()
	if len(bodies) != 1 {
		t.Fatalf("requests = %d, want 1", len(bodies))
	}
	if bodies[0] != `{"email":"admin","pass":"s3cret"}` {
		t.Errorf("body = %q", bodies[0])
	}
}

// TestTheSprayEscapesTheValueIntoTheBody: a quote in a password must not
// break the document the target has to parse.
func TestTheSprayEscapesTheValueIntoTheBody(t *testing.T) {
	hermetic(t)
	fake := &fakeProbe{}
	if _, err := Spray(context.Background(), SprayOptions{
		URL:         "https://example.com/login",
		Body:        `{"user":"{USERNAME}","password":"{PASSWORD}"}`,
		Credentials: []Credential{{Username: "admin", Password: `pa"ss`}},
		Probe:       fake.probe,
	}); err != nil {
		t.Fatalf("Spray: %v", err)
	}
	bodies := fake.bodies()
	if len(bodies) != 1 {
		t.Fatalf("requests = %d, want 1", len(bodies))
	}
	if !strings.Contains(bodies[0], `pa\"ss`) {
		t.Errorf("body = %q, want the quote escaped", bodies[0])
	}
}

// TestTheSprayRefusesABodyWithoutAPlaceholder: a template without a
// username would send the same request fifty times, and a template without a
// password would send the same password to fifty accounts.
func TestTheSprayRefusesABodyWithoutAPlaceholder(t *testing.T) {
	hermetic(t)
	for _, body := range []string{
		"",
		`{"user":"fixed","password":"fixed"}`,
		`{"user":"{USERNAME}","password":"fixed"}`,
	} {
		if _, err := Spray(context.Background(), SprayOptions{
			URL:         "https://example.com/login",
			Body:        body,
			Credentials: []Credential{{Username: "admin", Password: "s3cret"}},
			Probe:       (&fakeProbe{}).probe,
		}); err == nil {
			t.Errorf("Spray accepted the body %q", body)
		}
	}
}

// TestASuccessMarkerDecidesTheOutcome: a marker that only appears on
// success is the signal, and its absence is a failure even on a 200.
func TestASuccessMarkerDecidesTheOutcome(t *testing.T) {
	hermetic(t)
	fake := &fakeProbe{answer: func(req recon.Request) (Attempt, error) {
		return attemptFor(200, "<html>Welcome back, admin</html>"), nil
	}}
	outcomes, err := Spray(context.Background(), SprayOptions{
		URL:           "https://example.com/login",
		Body:          "username={USERNAME}&password={PASSWORD}",
		SuccessMarker: "welcome back",
		Credentials:   []Credential{{Username: "admin", Password: "s3cret"}},
		Probe:         fake.probe,
	})
	if err != nil {
		t.Fatalf("Spray: %v", err)
	}
	if len(outcomes) != 1 {
		t.Fatalf("outcomes = %d, want 1", len(outcomes))
	}
	if !outcomes[0].Success {
		t.Errorf("outcome = %+v, want a success", outcomes[0])
	}
	if !strings.Contains(outcomes[0].Evidence, "Welcome back") {
		t.Errorf("evidence = %q, want the excerpt around the marker", outcomes[0].Evidence)
	}
	if outcomes[0].Failure != "" {
		t.Errorf("failure = %q, want it empty on a success", outcomes[0].Failure)
	}
}

// TestAMissingSuccessMarkerIsAFailure: the marker is how a success is
// recognised, so its absence decides the attempt.
func TestAMissingSuccessMarkerIsAFailure(t *testing.T) {
	hermetic(t)
	fake := &fakeProbe{answer: func(req recon.Request) (Attempt, error) {
		return attemptFor(200, "<html>Sign in</html>"), nil
	}}
	outcomes, err := Spray(context.Background(), SprayOptions{
		URL:           "https://example.com/login",
		Body:          "username={USERNAME}&password={PASSWORD}",
		SuccessMarker: "welcome back",
		Credentials:   []Credential{{Username: "admin", Password: "s3cret"}},
		Probe:         fake.probe,
	})
	if err != nil {
		t.Fatalf("Spray: %v", err)
	}
	if outcomes[0].Success {
		t.Errorf("outcome = %+v, want a failure", outcomes[0])
	}
}

// TestTheSuccessFieldDecidesTheOutcome: a JSON login decides on a field.
func TestTheSuccessFieldDecidesTheOutcome(t *testing.T) {
	hermetic(t)
	cases := []struct {
		name  string
		body  string
		field string
		want  bool
	}{
		{name: "boolean true", body: `{"data":{"authenticated":true}}`, field: "data.authenticated", want: true},
		{name: "boolean false", body: `{"data":{"authenticated":false}}`, field: "data.authenticated", want: false},
		{name: "string ok", body: `{"status":"ok"}`, field: "status", want: true},
		{name: "string failed", body: `{"status":"failed"}`, field: "status", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeProbe{answer: func(req recon.Request) (Attempt, error) {
				return attemptFor(200, tc.body), nil
			}}
			outcomes, err := Spray(context.Background(), SprayOptions{
				URL:          "https://example.com/login",
				Body:         `{"user":"{USERNAME}","password":"{PASSWORD}"}`,
				SuccessField: tc.field,
				Credentials:  []Credential{{Username: "admin", Password: "s3cret"}},
				Probe:        fake.probe,
			})
			if err != nil {
				t.Fatalf("Spray: %v", err)
			}
			if outcomes[0].Success != tc.want {
				t.Errorf("success = %t, want %t (outcome %+v)", outcomes[0].Success, tc.want, outcomes[0])
			}
		})
	}
}

// TestTheSuccessFieldReadsANestedField: the field may be a dotted path.
func TestTheSuccessFieldReadsANestedField(t *testing.T) {
	hermetic(t)
	fake := &fakeProbe{answer: func(req recon.Request) (Attempt, error) {
		return attemptFor(200, `{"data":{"session":{"ok":true}}}`), nil
	}}
	outcomes, err := Spray(context.Background(), SprayOptions{
		URL:          "https://example.com/login",
		Body:         `{"user":"{USERNAME}","password":"{PASSWORD}"}`,
		SuccessField: "data.session.ok",
		Credentials:  []Credential{{Username: "admin", Password: "s3cret"}},
		Probe:        fake.probe,
	})
	if err != nil {
		t.Fatalf("Spray: %v", err)
	}
	if !outcomes[0].Success {
		t.Errorf("outcome = %+v, want a success", outcomes[0])
	}
}

// TestTheMarkersNameTheReason: a locked account and a wrong password are
// different findings, and a report that cannot tell them apart sends the
// reader after the wrong thing.
func TestTheMarkersNameTheReason(t *testing.T) {
	hermetic(t)
	cases := []struct {
		name    string
		body    string
		markers []string
		locked  []string
		want    string
	}{
		{
			name:    "wrong password",
			body:    "<p>Invalid password</p>",
			markers: []string{"invalid password"},
			want:    FailureWrongPassword,
		},
		{
			name:   "locked",
			body:   "<p>Your account is locked</p>",
			locked: []string{"account is locked"},
			want:   FailureLocked,
		},
		{
			name:    "captcha",
			body:    "<p>Please complete the captcha</p>",
			markers: []string{"complete the captcha"},
			want:    FailureCaptcha,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeProbe{answer: func(req recon.Request) (Attempt, error) {
				return attemptFor(401, tc.body), nil
			}}
			outcomes, err := Spray(context.Background(), SprayOptions{
				URL:            "https://example.com/login",
				Body:           "username={USERNAME}&password={PASSWORD}",
				FailureMarkers: tc.markers,
				LockedMarkers:  tc.locked,
				Credentials:    []Credential{{Username: "admin", Password: "s3cret"}},
				Probe:          fake.probe,
			})
			if err != nil {
				t.Fatalf("Spray: %v", err)
			}
			if outcomes[0].Success {
				t.Errorf("outcome = %+v, want a failure", outcomes[0])
			}
			if outcomes[0].Failure != tc.want {
				t.Errorf("failure = %q, want %q", outcomes[0].Failure, tc.want)
			}
			if outcomes[0].Evidence == "" {
				t.Error("the outcome carries no evidence")
			}
		})
	}
}

// TestTheRateLimitStatusNamesTheReason: a target asking for less traffic
// is not a rejected credential.
func TestTheRateLimitStatusNamesTheReason(t *testing.T) {
	hermetic(t)
	fake := &fakeProbe{answer: func(req recon.Request) (Attempt, error) {
		return attemptFor(429, "slow down"), nil
	}}
	outcomes, err := Spray(context.Background(), SprayOptions{
		URL:         "https://example.com/login",
		Body:        "username={USERNAME}&password={PASSWORD}",
		Credentials: []Credential{{Username: "admin", Password: "s3cret"}},
		Probe:       fake.probe,
	})
	if err != nil {
		t.Fatalf("Spray: %v", err)
	}
	if outcomes[0].Failure != FailureRateLimited {
		t.Errorf("failure = %q, want %q", outcomes[0].Failure, FailureRateLimited)
	}
}

// TestTheRateLimitStatusIsConfigurable: not every target answers 429.
func TestTheRateLimitStatusIsConfigurable(t *testing.T) {
	hermetic(t)
	fake := &fakeProbe{answer: func(req recon.Request) (Attempt, error) {
		return attemptFor(503, "busy"), nil
	}}
	outcomes, err := Spray(context.Background(), SprayOptions{
		URL:             "https://example.com/login",
		Body:            "username={USERNAME}&password={PASSWORD}",
		RateLimitStatus: 503,
		Credentials:     []Credential{{Username: "admin", Password: "s3cret"}},
		Probe:           fake.probe,
	})
	if err != nil {
		t.Fatalf("Spray: %v", err)
	}
	if outcomes[0].Failure != FailureRateLimited {
		t.Errorf("failure = %q, want %q", outcomes[0].Failure, FailureRateLimited)
	}
}

// TestAServerErrorIsNotARejectedCredential: a target-side failure is not
// evidence about the password.
func TestAServerErrorIsNotARejectedCredential(t *testing.T) {
	hermetic(t)
	fake := &fakeProbe{answer: func(req recon.Request) (Attempt, error) {
		return attemptFor(500, "boom"), nil
	}}
	outcomes, err := Spray(context.Background(), SprayOptions{
		URL:         "https://example.com/login",
		Body:        "username={USERNAME}&password={PASSWORD}",
		Credentials: []Credential{{Username: "admin", Password: "s3cret"}},
		Probe:       fake.probe,
	})
	if err != nil {
		t.Fatalf("Spray: %v", err)
	}
	if outcomes[0].Failure != FailureServerError {
		t.Errorf("failure = %q, want %q", outcomes[0].Failure, FailureServerError)
	}
}

// TestThePerUserCeilingStopsTheSpray: a spray that locks the accounts it is
// testing ends the engagement, so the ceiling is enforced and reported.
func TestThePerUserCeilingStopsTheSpray(t *testing.T) {
	hermetic(t)
	fake := &fakeProbe{}
	credentials := make([]Credential, 0, 8)
	for _, user := range []string{"alice", "bob"} {
		for _, password := range []string{"one", "two", "three", "four"} {
			credentials = append(credentials, Credential{Username: user, Password: password})
		}
	}
	result, err := SprayDetailed(context.Background(), SprayOptions{
		URL:                "https://example.com/login",
		Body:               "username={USERNAME}&password={PASSWORD}",
		MaxAttemptsPerUser: 2,
		Credentials:        credentials,
		Probe:              fake.probe,
	})
	if err != nil {
		t.Fatalf("SprayDetailed: %v", err)
	}
	if len(fake.requests) != 4 {
		t.Errorf("requests = %d, want 4 (two accounts, two attempts each)", len(fake.requests))
	}
	if result.Summary.Attempts != 4 {
		t.Errorf("attempts = %d, want 4", result.Summary.Attempts)
	}
	if result.Summary.UsersStopped != 2 {
		t.Errorf("users stopped = %d, want 2", result.Summary.UsersStopped)
	}
	if !strings.Contains(result.Summary.String(), "per-user ceiling") {
		t.Errorf("summary = %q, want it to name the ceiling", result.Summary.String())
	}
}

// TestASprayStopsAfterAnAccountIsIn: trying more passwords against an
// account that already authenticated is what trips a lock.
func TestASprayStopsAfterAnAccountIsIn(t *testing.T) {
	hermetic(t)
	fake := &fakeProbe{answer: func(req recon.Request) (Attempt, error) {
		return attemptFor(200, "Welcome back"), nil
	}}
	result, err := SprayDetailed(context.Background(), SprayOptions{
		URL:                "https://example.com/login",
		Body:               "username={USERNAME}&password={PASSWORD}",
		SuccessMarker:      "welcome back",
		MaxAttemptsPerUser: 5,
		Credentials: []Credential{
			{Username: "alice", Password: "one"},
			{Username: "alice", Password: "two"},
		},
		Probe: fake.probe,
	})
	if err != nil {
		t.Fatalf("SprayDetailed: %v", err)
	}
	if len(fake.requests) != 1 {
		t.Errorf("requests = %d, want 1", len(fake.requests))
	}
	if result.Summary.Successes != 1 {
		t.Errorf("successes = %d, want 1", result.Summary.Successes)
	}
}

// TestASuccessIsStoredAsASession: the point of a successful credential is
// the session that follows it.
func TestASuccessIsStoredAsASession(t *testing.T) {
	hermetic(t)
	store, err := jar.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	fake := &fakeProbe{answer: func(req recon.Request) (Attempt, error) {
		attempt := attemptFor(200, "Welcome back")
		attempt.Cookies = []jar.Cookie{{Name: "sessionid", Value: "s3cret-cookie", Domain: "example.com", Path: "/"}}
		return attempt, nil
	}}
	result, err := SprayDetailed(context.Background(), SprayOptions{
		URL:           "https://example.com/login",
		Body:          "username={USERNAME}&password={PASSWORD}",
		SuccessMarker: "welcome back",
		Credentials:   []Credential{{Username: "admin", Password: "s3cret"}},
		Store:         store,
		SessionName:   "spray",
		Probe:         fake.probe,
	})
	if err != nil {
		t.Fatalf("SprayDetailed: %v", err)
	}
	if len(result.Summary.Sessions) != 1 || result.Summary.Sessions[0] != "spray" {
		t.Errorf("sessions = %v, want [spray]", result.Summary.Sessions)
	}
	sess, err := store.Load("spray")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(sess.Cookies) != 1 || sess.Cookies[0].Name != "sessionid" {
		t.Errorf("cookies = %+v, want the cookie of the response", sess.Cookies)
	}
	if sess.Meta["username"] != "admin" {
		t.Errorf("meta = %v, want the username", sess.Meta)
	}
}

// TestTheSessionIsCapturedWhenTheProbeCarriesNoCookies: the probe records
// that a cookie was set without recording its value, so the session is
// captured with one extra authenticated request.
func TestTheSessionIsCapturedWhenTheProbeCarriesNoCookies(t *testing.T) {
	hermetic(t)
	store, err := jar.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	fake := &fakeProbe{answer: func(req recon.Request) (Attempt, error) {
		return attemptFor(200, "Welcome back"), nil
	}}
	captured := 0
	capture := func(ctx context.Context, req recon.Request) ([]jar.Cookie, error) {
		captured++
		return []jar.Cookie{{Name: "sessionid", Value: "from-capture", Domain: "example.com", Path: "/"}}, nil
	}
	if _, err := SprayDetailed(context.Background(), SprayOptions{
		URL:           "https://example.com/login",
		Body:          "username={USERNAME}&password={PASSWORD}",
		SuccessMarker: "welcome back",
		Credentials:   []Credential{{Username: "admin", Password: "s3cret"}},
		Store:         store,
		SessionName:   "spray",
		Probe:         fake.probe,
		Capture:       capture,
	}); err != nil {
		t.Fatalf("SprayDetailed: %v", err)
	}
	if captured != 1 {
		t.Errorf("captures = %d, want 1", captured)
	}
	sess, err := store.Load("spray")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(sess.Cookies) != 1 || sess.Cookies[0].Value != "from-capture" {
		t.Errorf("cookies = %+v, want the captured cookie", sess.Cookies)
	}
}

// TestTheSessionNameIsDerivedFromTheUsername: a spray can authenticate more
// than one account, and a session store holds one account per name.
func TestTheSessionNameIsDerivedFromTheUsername(t *testing.T) {
	hermetic(t)
	store, err := jar.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	fake := &fakeProbe{answer: func(req recon.Request) (Attempt, error) {
		return attemptFor(200, "Welcome back"), nil
	}}
	if _, err := SprayDetailed(context.Background(), SprayOptions{
		URL:           "https://example.com/login",
		Body:          "username={USERNAME}&password={PASSWORD}",
		SuccessMarker: "welcome back",
		Credentials: []Credential{
			{Username: "alice@example.com", Password: "one"},
			{Username: "bob", Password: "two"},
		},
		Store: store,
		Probe: fake.probe,
	}); err != nil {
		t.Fatalf("SprayDetailed: %v", err)
	}
	if _, err := store.Load("cred-alice-example.com"); err != nil {
		t.Errorf("the first session is not under its derived name: %v", err)
	}
	if _, err := store.Load("cred-bob"); err != nil {
		t.Errorf("the second session is not under its derived name: %v", err)
	}
}

// TestThePasswordNeverReachesTheOutput: the report of a credential test is
// pasted into a disclosure, and a disclosure that carries the password is a
// second copy of the secret.
func TestThePasswordNeverReachesTheOutput(t *testing.T) {
	hermetic(t)
	const password = "sup3rs3cret"
	fake := &fakeProbe{answer: func(req recon.Request) (Attempt, error) {
		// The target echoes the submitted password back, which is what an
		// error page does when it says which value was wrong.
		return attemptFor(401, "<p>The password "+password+" is wrong</p>"), nil
	}}
	result, err := SprayDetailed(context.Background(), SprayOptions{
		URL:         "https://example.com/login",
		Body:        "username={USERNAME}&password={PASSWORD}",
		Credentials: []Credential{{Username: "admin", Password: password}},
		Probe:       fake.probe,
	})
	if err != nil {
		t.Fatalf("SprayDetailed: %v", err)
	}
	if len(result.Outcomes) != 1 {
		t.Fatalf("outcomes = %d, want 1", len(result.Outcomes))
	}
	line := result.Outcomes[0].String()
	if strings.Contains(line, password) {
		t.Errorf("the outcome carries the password: %q", line)
	}
	if !strings.Contains(line, "admin") {
		t.Errorf("the outcome does not carry the username: %q", line)
	}
	if strings.Contains(result.Summary.String(), password) {
		t.Errorf("the summary carries the password: %q", result.Summary.String())
	}
}

// TestTheRefusedAttemptIsNotSent: the gate decides whether a destination may
// be contacted at all, and a refused attempt never leaves the machine.
func TestTheRefusedAttemptIsNotSent(t *testing.T) {
	hermetic(t)
	fake := &fakeProbe{}
	result, err := SprayDetailed(context.Background(), SprayOptions{
		// The host's own machine is refused by the gate with no
		// engagement on record.
		URL:         "http://127.0.0.1/login",
		Body:        "username={USERNAME}&password={PASSWORD}",
		Credentials: []Credential{{Username: "admin", Password: "s3cret"}},
		Probe:       fake.probe,
	})
	if err != nil {
		t.Fatalf("SprayDetailed: %v", err)
	}
	if len(fake.requests) != 0 {
		t.Errorf("requests = %d, want none", len(fake.requests))
	}
	if result.Summary.Refused != 1 {
		t.Errorf("refused = %d, want 1", result.Summary.Refused)
	}
}

// TestAGetLoginCarriesTheCredentialsInTheQuery: a GET has no body.
func TestAGetLoginCarriesTheCredentialsInTheQuery(t *testing.T) {
	hermetic(t)
	fake := &fakeProbe{}
	if _, err := Spray(context.Background(), SprayOptions{
		URL:         "https://example.com/login",
		Method:      "GET",
		Body:        "username={USERNAME}&password={PASSWORD}",
		Credentials: []Credential{{Username: "admin", Password: "s3cret"}},
		Probe:       fake.probe,
	}); err != nil {
		t.Fatalf("Spray: %v", err)
	}
	urls := fake.urls()
	if len(urls) != 1 {
		t.Fatalf("requests = %d, want 1", len(urls))
	}
	if !strings.Contains(urls[0], "username=admin") || !strings.Contains(urls[0], "password=s3cret") {
		t.Errorf("url = %q, want the credentials in the query", urls[0])
	}
	if body := fake.bodies(); len(body) != 1 || body[0] != "" {
		t.Errorf("body = %q, want it empty on a GET", body)
	}
}

// TestTheSprayRefusesAnImpossibleRequest: a method a login does not have, a
// url that is not one, and a field name that cannot carry two values.
func TestTheSprayRefusesAnImpossibleRequest(t *testing.T) {
	hermetic(t)
	cases := []SprayOptions{
		{URL: "", Body: "username={USERNAME}&password={PASSWORD}"},
		{URL: "not a url", Body: "username={USERNAME}&password={PASSWORD}"},
		{URL: "ftp://example.com/login", Body: "username={USERNAME}&password={PASSWORD}"},
		{URL: "https://example.com/login", Method: "DELETE", Body: "username={USERNAME}&password={PASSWORD}"},
		{
			URL:           "https://example.com/login",
			Body:          "username={USERNAME}&password={PASSWORD}",
			UsernameField: "user",
			PasswordField: "user",
		},
	}
	for _, opts := range cases {
		opts.Credentials = []Credential{{Username: "admin", Password: "s3cret"}}
		opts.Probe = (&fakeProbe{}).probe
		if _, err := Spray(context.Background(), opts); err == nil {
			t.Errorf("Spray accepted %+v", opts)
		}
	}
}

// TestTheSprayNeedsCredentials: a spray of nothing is not a spray.
func TestTheSprayNeedsCredentials(t *testing.T) {
	hermetic(t)
	if _, err := Spray(context.Background(), SprayOptions{
		URL:   "https://example.com/login",
		Body:  "username={USERNAME}&password={PASSWORD}",
		Probe: (&fakeProbe{}).probe,
	}); err == nil {
		t.Error("Spray accepted a run with no credentials")
	}
}

// TestARequestErrorIsReportedNotSwallowed: a spray that fails on every
// attempt must say so.
func TestARequestErrorIsReportedNotSwallowed(t *testing.T) {
	hermetic(t)
	fake := &fakeProbe{answer: func(req recon.Request) (Attempt, error) {
		return Attempt{}, context.DeadlineExceeded
	}}
	result, err := SprayDetailed(context.Background(), SprayOptions{
		URL:         "https://example.com/login",
		Body:        "username={USERNAME}&password={PASSWORD}",
		Credentials: []Credential{{Username: "admin", Password: "s3cret"}},
		Probe:       fake.probe,
	})
	if err != nil {
		t.Fatalf("SprayDetailed: %v", err)
	}
	if len(result.Summary.Errors) == 0 {
		t.Fatal("the error was swallowed")
	}
	if !strings.Contains(result.Summary.Errors[0], "admin") {
		t.Errorf("error = %q, want it to name the account", result.Summary.Errors[0])
	}
}

// TestTheSummaryCountsTheFailuresByReason: a report has to say which
// failure it was, not only how many.
func TestTheSummaryCountsTheFailuresByReason(t *testing.T) {
	hermetic(t)
	fake := &fakeProbe{answer: func(req recon.Request) (Attempt, error) {
		if strings.Contains(req.Body, "locked") {
			return attemptFor(403, "account locked"), nil
		}
		return attemptFor(401, "invalid password"), nil
	}}
	result, err := SprayDetailed(context.Background(), SprayOptions{
		URL:            "https://example.com/login",
		Body:           "username={USERNAME}&password={PASSWORD}",
		FailureMarkers: []string{"invalid password"},
		LockedMarkers:  []string{"locked"},
		Credentials:    []Credential{{Username: "admin", Password: "s3cret"}, {Username: "bob", Password: "locked"}},
		Probe:          fake.probe,
	})
	if err != nil {
		t.Fatalf("SprayDetailed: %v", err)
	}
	if result.Summary.Failures[FailureWrongPassword] != 1 {
		t.Errorf("failures = %v, want one wrong password", result.Summary.Failures)
	}
	if result.Summary.Failures[FailureLocked] != 1 {
		t.Errorf("failures = %v, want one locked", result.Summary.Failures)
	}
}
