// Package creds tests a list of credentials against a login endpoint.
//
// The gap it closes: the recon layer could log in once and replay, but it
// could not test a list of credentials, could not tell a locked account from
// a wrong password, and could not turn a successful login into a session it
// could keep using. That is the difference between testing one account and
// testing an account list, which is where a lot of real findings start.
//
// Three rules shape the package:
//
//   - Every attempt is gated, budgeted and receipted individually, exactly
//     like a probe. A spray of fifty credentials is fifty authorized
//     requests, not one privileged first hop with forty-nine followers.
//   - The per-user ceiling is the point of the whole thing. A spray that
//     locks the accounts it is testing ends the engagement, so the number of
//     attempts per user is capped and the cap is reported.
//   - A password never reaches the output. The report of a credential test
//     gets pasted into a disclosure, and a disclosure that carries the
//     password it found is a second copy of the secret. The username is
//     printed; the password is not, in any code path.
package creds

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"ceraa/harness/jar"
	"ceraa/harness/netguard"
	"ceraa/harness/recon"
)

// The bounds. A caller may ask for less; nobody may ask for more.
const (
	// defaultBodyCap bounds what a spray reads from a response. A login
	// response is a page, and the cap keeps one response from becoming a
	// large allocation.
	defaultBodyCap = 64 * 1024
	// maxEvidenceRunes bounds the excerpt kept around a signal.
	maxEvidenceRunes = 160
	// evidenceContextRunes is how much text sits on either side of the
	// signal inside the excerpt.
	evidenceContextRunes = 60
	// defaultMaxAttemptsPerUser is the ceiling on the attempts one account
	// gets. Three is the number a lockout policy usually tolerates, and a
	// ceiling that is not there is a spray that ends the engagement.
	defaultMaxAttemptsPerUser = 3
	// hardMaxAttemptsPerUser is the most attempts one account may get.
	hardMaxAttemptsPerUser = 20
	// defaultConcurrency is one. A spray is the one place where parallel
	// requests hurt: the ceiling protects the accounts, but only a
	// sequential spray keeps the attempts of one account adjacent, which is
	// what a lockout counter counts.
	defaultConcurrency = 1
	// hardConcurrency is the most workers a spray may run.
	hardConcurrency = 8
	// hardDelayMS is the longest pause between attempts.
	hardDelayMS = 60000
	// maxSprayAttempts bounds the whole run.
	maxSprayAttempts = 5000
	// maxReportedErrors is how many request errors the summary keeps.
	maxReportedErrors = 10
	// maxSessionNameLen matches the jar's own limit.
	maxSessionNameLen = 64
)

// The failure reasons. A report that says "login failed" for every attempt
// is a report nobody can act on: a locked account and a wrong password are
// different findings with different fixes.
const (
	// FailureWrongPassword is the rejection a credential earns.
	FailureWrongPassword = "wrong password"
	// FailureLocked is an account the target has locked.
	FailureLocked = "locked"
	// FailureCaptcha is a challenge in front of the login.
	FailureCaptcha = "captcha"
	// FailureRateLimited is the target asking for less traffic.
	FailureRateLimited = "rate limited"
	// FailureServerError is a target-side failure, not a credential answer.
	FailureServerError = "server error"
)

// Credential is one username and password pair.
type Credential struct {
	Username string
	Password string
	// Source names where the pair came from: "wordlist", "combo" or
	// "explicit". It travels into the outcome so a report can say where a
	// hit was found.
	Source string
}

// Outcome is what one attempt produced.
type Outcome struct {
	Credential Credential
	Status     int
	// Success is true when the response says the credentials worked.
	Success bool
	// Failure names the reason the attempt did not work: "wrong password",
	// "locked", "captcha", "rate limited", "server error".
	Failure string
	// Evidence is a bounded excerpt around the signal.
	Evidence  string
	ElapsedMS int64
	SHA256    string
	ReceiptID string
}

// String renders the outcome for a report.
//
// The username is in it and the password is not: this string is what ends up
// in a disclosure.
func (o Outcome) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "user %s -> %d", o.Credential.Username, o.Status)
	if o.Success {
		b.WriteString(" authenticated")
	} else if o.Failure != "" {
		fmt.Fprintf(&b, " %s", o.Failure)
	}
	if o.Evidence != "" {
		fmt.Fprintf(&b, " (evidence: %q)", o.Evidence)
	}
	return b.String()
}

// ProbeFunc is the request seam the spray uses.
//
// The default is the real probe: gated, budgeted, receipted and bounded. A
// test injects a plain one so the suite exercises the spray logic without
// touching the network.
type ProbeFunc func(ctx context.Context, req recon.Request, bodyCap int) (Attempt, error)

// CaptureFunc returns the session cookies of a successful login.
//
// The default authenticates once more through the guarded transport,
// because the probe deliberately does not carry cookie values: a receipt is
// evidence, not a warehouse, and a session without its cookies is not a
// session. A test injects a plain one so the suite never reaches the
// network.
type CaptureFunc func(ctx context.Context, req recon.Request) ([]jar.Cookie, error)

// Attempt is one observation: the recon result, the bounded body and the
// cookies the response set.
type Attempt struct {
	Result  recon.Result
	Body    []byte
	Cookies []jar.Cookie
}

// SprayOptions bounds one spray.
type SprayOptions struct {
	// URL is the login endpoint.
	URL string
	// Method is POST or GET, default POST.
	Method string
	// Body is the request template. {USERNAME} and {PASSWORD} are replaced.
	Body    string
	Headers map[string]string
	// UsernameField names the field the username travels in, default
	// "username". It is a placeholder in the template as well, so a JSON
	// login reads {"email":"{USERNAME}","password":"{PASSWORD}"} and a form
	// login reads username={USERNAME}&password={PASSWORD}.
	UsernameField string
	// PasswordField names the field the password travels in, default
	// "password".
	PasswordField string
	// SuccessMarker is a substring that only appears on success. When it is
	// set, its absence is a failure.
	SuccessMarker string
	// SuccessField is the JSON field whose value decides success, dotted
	// for a nested field ("data.authenticated"). When it is set, the field
	// has to be there and has to decide success.
	SuccessField string
	// FailureMarkers are substrings that mean the attempt was rejected.
	FailureMarkers []string
	// LockedMarkers are substrings that mean the account is locked.
	LockedMarkers []string
	// RateLimitStatus is the status that means the target is rate limiting.
	// Zero means 429.
	RateLimitStatus int
	// Credentials are the pairs to try.
	Credentials []Credential
	// MaxAttemptsPerUser stops a spray that would lock accounts. Zero means
	// the default of three.
	MaxAttemptsPerUser int
	// DelayMS is the pause between attempts.
	DelayMS int
	// Concurrency is the number of parallel attempts. Zero means one.
	Concurrency int
	// Timeout is the per-request timeout in seconds.
	Timeout int
	// Store, when set, receives the session of a successful login.
	Store *jar.Store
	// SessionName is the name the session is stored under. Empty derives it
	// from the username.
	SessionName string
	// Probe is the request seam. Nil uses the real probe.
	Probe ProbeFunc
	// Capture returns the session cookies of a successful login. Nil uses
	// the real capture.
	Capture CaptureFunc
}

// Summary is what one spray did, in the numbers a report needs.
//
// A spray that only returned outcomes could not say how much was tried, how
// many accounts the ceiling cut off, or whether the target rate limited the
// run. A report that cannot say that is a report nobody trusts.
type Summary struct {
	// Attempts is how many credentials were tried.
	Attempts int
	// Successes is how many of them authenticated.
	Successes int
	// Failures counts the attempts that did not work, by reason.
	Failures map[string]int
	// UsersStopped is how many accounts were cut off by the per-user
	// ceiling.
	UsersStopped int
	// RateLimited is true when a rate limit or a budget stopped the spray.
	RateLimited bool
	// RateLimit names the limit that stopped it.
	RateLimit string
	// Refused counts the attempts the scope gate refused before anything
	// was dialed.
	Refused int
	// Errors holds the first few request errors, bounded.
	Errors []string
	// Sessions names the sessions stored for the logins that worked.
	Sessions []string
}

// String renders the summary for a report.
func (s Summary) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d attempts, %d successes", s.Attempts, s.Successes)
	if len(s.Failures) > 0 {
		reasons := make([]string, 0, len(s.Failures))
		total := 0
		for reason, count := range s.Failures {
			reasons = append(reasons, fmt.Sprintf("%s %d", reason, count))
			total += count
		}
		sort.Strings(reasons)
		fmt.Fprintf(&b, ", %d failures (%s)", total, strings.Join(reasons, ", "))
	}
	if s.UsersStopped > 0 {
		fmt.Fprintf(&b, ", %d accounts stopped by the per-user ceiling", s.UsersStopped)
	}
	if s.RateLimited {
		fmt.Fprintf(&b, ", rate limit hit: %s", s.RateLimit)
	}
	if s.Refused > 0 {
		fmt.Fprintf(&b, ", %d refused by scope", s.Refused)
	}
	return b.String()
}

// Result is a spray: the outcomes and the summary of what it did.
type Result struct {
	Outcomes []Outcome
	Summary  Summary
}

// Spray tries the credentials and returns what each attempt produced.
func Spray(ctx context.Context, opts SprayOptions) ([]Outcome, error) {
	result, err := SprayDetailed(ctx, opts)
	if err != nil {
		return nil, err
	}
	return result.Outcomes, nil
}

// SprayDetailed runs the spray and returns the outcomes with the summary.
func SprayDetailed(ctx context.Context, opts SprayOptions) (Result, error) {
	plan, err := planSpray(opts)
	if err != nil {
		return Result{}, err
	}
	if len(plan.credentials) == 0 {
		return Result{}, fmt.Errorf("there are no credentials to try")
	}

	state := &sprayState{
		maxPerUser:    plan.maxPerUser,
		attempts:      make(map[string]int),
		stopped:       make(map[string]bool),
		authenticated: make(map[string]bool),
		usedNames:     make(map[string]bool),
		failures:      make(map[string]int),
	}

	workers := plan.concurrency
	if workers > len(plan.credentials) {
		workers = len(plan.credentials)
	}
	queue := make(chan Credential)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for cred := range queue {
				if ctx.Err() != nil {
					return
				}
				state.attempt(ctx, cred, plan)
			}
		}()
	}
	for _, cred := range plan.credentials {
		if ctx.Err() != nil {
			state.setStopped("context cancelled")
			break
		}
		if state.isStopped() {
			break
		}
		select {
		case queue <- cred:
		case <-ctx.Done():
		}
	}
	close(queue)
	wg.Wait()

	outcomes := state.outcomesSnapshot()
	sort.SliceStable(outcomes, func(i, j int) bool {
		return outcomes[i].Credential.Username < outcomes[j].Credential.Username
	})

	summary := state.summarySnapshot()
	return Result{Outcomes: outcomes, Summary: summary}, nil
}

// ---------------------------------------------------------------------------
// The plan
// ---------------------------------------------------------------------------

// sprayPlan is the resolved form of the options: everything the workers need,
// decided once so two workers cannot disagree about it.
type sprayPlan struct {
	url             string
	method          string
	headers         map[string]string
	body            *bodyTemplate
	usernameField   string
	passwordField   string
	successMarker   string
	successField    string
	failureMarkers  []string
	lockedMarkers   []string
	rateLimitStatus int
	maxPerUser      int
	delay           time.Duration
	concurrency     int
	timeout         int
	bodyCap         int
	credentials     []Credential
	// store receives the session of a successful login.
	store *jar.Store
	// sessionBase is the name the session is stored under, before the
	// username is folded in.
	sessionBase string
	// probe is the request seam and capture the session seam. Both are
	// resolved here so a worker never has to decide.
	probe   ProbeFunc
	capture CaptureFunc
}

// planSpray validates the options and resolves them into the plan.
//
// The validation is where a mistyped request is caught: a body template
// without a username placeholder would send the same request fifty times,
// and a body template without a password placeholder would send the same
// password to fifty accounts.
func planSpray(opts SprayOptions) (*sprayPlan, error) {
	target := strings.TrimSpace(opts.URL)
	if target == "" {
		return nil, fmt.Errorf("url is required")
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("url %q is not a valid url: %w", target, err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if (scheme != "http" && scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("url %q must be an absolute http:// or https:// url", target)
	}

	method := strings.ToUpper(strings.TrimSpace(opts.Method))
	if method == "" {
		method = http.MethodPost
	}
	if method != http.MethodPost && method != http.MethodGet {
		// Only the two verbs a login has. A login that needs a PUT is a
		// conversation with the target, not a spray.
		return nil, fmt.Errorf("method %q is not one of POST, GET", opts.Method)
	}

	usernameField := strings.TrimSpace(opts.UsernameField)
	if usernameField == "" {
		usernameField = "username"
	}
	passwordField := strings.TrimSpace(opts.PasswordField)
	if passwordField == "" {
		passwordField = "password"
	}
	if strings.EqualFold(usernameField, passwordField) {
		return nil, fmt.Errorf("the username field and the password field are the same name (%q); a login sends two values", usernameField)
	}

	body, err := compileBody(opts.Body, usernameField, passwordField)
	if err != nil {
		return nil, err
	}

	headers := make(map[string]string, len(opts.Headers)+1)
	for name, value := range opts.Headers {
		headers[name] = value
	}
	if body.isJSON && !hasHeader(headers, "Content-Type") {
		// The probe sends the body as given, and a JSON body without its
		// content type is a body the target has to guess at.
		headers["Content-Type"] = "application/json"
	} else if !body.isJSON && !hasHeader(headers, "Content-Type") {
		headers["Content-Type"] = "application/x-www-form-urlencoded"
	}

	maxPerUser := opts.MaxAttemptsPerUser
	if maxPerUser <= 0 {
		maxPerUser = defaultMaxAttemptsPerUser
	}
	if maxPerUser > hardMaxAttemptsPerUser {
		maxPerUser = hardMaxAttemptsPerUser
	}

	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}
	if concurrency > hardConcurrency {
		concurrency = hardConcurrency
	}

	delay := time.Duration(opts.DelayMS) * time.Millisecond
	if delay < 0 {
		delay = 0
	}
	if delay > time.Duration(hardDelayMS)*time.Millisecond {
		delay = time.Duration(hardDelayMS) * time.Millisecond
	}

	rateLimitStatus := opts.RateLimitStatus
	if rateLimitStatus <= 0 {
		rateLimitStatus = http.StatusTooManyRequests
	}

	probe := opts.Probe
	if probe == nil {
		probe = defaultProbe
	}
	capture := opts.Capture
	if capture == nil {
		capture = defaultCapture
	}

	credentials := make([]Credential, 0, len(opts.Credentials))
	for _, cred := range opts.Credentials {
		if strings.TrimSpace(cred.Username) == "" {
			continue
		}
		credentials = append(credentials, cred)
		if len(credentials) >= maxSprayAttempts {
			break
		}
	}

	return &sprayPlan{
		url:             target,
		method:          method,
		headers:         headers,
		body:            body,
		usernameField:   usernameField,
		passwordField:   passwordField,
		successMarker:   strings.TrimSpace(opts.SuccessMarker),
		successField:    strings.TrimSpace(opts.SuccessField),
		failureMarkers:  cleanMarkers(opts.FailureMarkers),
		lockedMarkers:   cleanMarkers(opts.LockedMarkers),
		rateLimitStatus: rateLimitStatus,
		maxPerUser:      maxPerUser,
		delay:           delay,
		concurrency:     concurrency,
		timeout:         opts.Timeout,
		bodyCap:         defaultBodyCap,
		credentials:     credentials,
		store:           opts.Store,
		sessionBase:     strings.TrimSpace(opts.SessionName),
		probe:           probe,
		capture:         capture,
	}, nil
}

// hasHeader reports whether a header name is already set, ignoring case.
func hasHeader(headers map[string]string, name string) bool {
	for key := range headers {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

// cleanMarkers trims the markers and drops the empty ones.
func cleanMarkers(markers []string) []string {
	out := make([]string, 0, len(markers))
	for _, marker := range markers {
		trimmed := strings.TrimSpace(marker)
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

// ---------------------------------------------------------------------------
// The body template
// ---------------------------------------------------------------------------

// bodyTemplate is the request body with its placeholders resolved.
type bodyTemplate struct {
	text string
	// isJSON decides how a value is escaped into the body.
	isJSON bool
	// userNames and passNames are the placeholder names that carry each
	// value: the generic one and the field the caller named.
	userNames []string
	passNames []string
}

// compileBody reads the template and works out which placeholders it has.
//
// Both the generic placeholders and the caller's field names are accepted,
// so a JSON login and a form login work without the caller having to know
// which spelling the engine prefers.
func compileBody(template, usernameField, passwordField string) (*bodyTemplate, error) {
	text := template
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("a body template is required: the spray sends the credentials in it")
	}
	trimmed := strings.TrimSpace(text)
	userNames := []string{"USERNAME"}
	if usernameField != "" {
		userNames = append(userNames, usernameField)
	}
	passNames := []string{"PASSWORD"}
	if passwordField != "" {
		passNames = append(passNames, passwordField)
	}

	if !hasPlaceholder(text, userNames) {
		return nil, fmt.Errorf("the body template has no username placeholder: it needs {USERNAME} or {%s}", usernameField)
	}
	if !hasPlaceholder(text, passNames) {
		return nil, fmt.Errorf("the body template has no password placeholder: it needs {PASSWORD} or {%s}", passwordField)
	}

	return &bodyTemplate{
		text:      text,
		isJSON:    strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "["),
		userNames: userNames,
		passNames: passNames,
	}, nil
}

// hasPlaceholder reports whether the template carries any of the names.
func hasPlaceholder(text string, names []string) bool {
	for _, name := range names {
		if name == "" {
			continue
		}
		if strings.Contains(text, "{"+name+"}") {
			return true
		}
	}
	return false
}

// render puts one credential into the template.
func (t *bodyTemplate) render(username, password string) string {
	out := t.text
	for _, name := range t.userNames {
		out = strings.ReplaceAll(out, "{"+name+"}", t.escape(username))
	}
	for _, name := range t.passNames {
		out = strings.ReplaceAll(out, "{"+name+"}", t.escape(password))
	}
	return out
}

// escape fits a value into the body.
//
// A JSON body gets a JSON string, because a quote in a password would
// otherwise break the document the target has to parse. Anything else gets
// form encoding, because that is what a form login is read as.
func (t *bodyTemplate) escape(value string) string {
	if t.isJSON {
		return jsonString(value)
	}
	return url.QueryEscape(value)
}

// jsonString escapes a value for use inside a JSON string literal. The
// quotes are left to the template, so the placeholder sits inside them.
func jsonString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	return string(encoded[1 : len(encoded)-1])
}

// requestURL builds the URL for one credential.
//
// A GET login carries the credentials in the query string, because a GET has
// no body. The password is then part of the URL, and the URL is what the
// receipt cites, so a POST is the better shape wherever the target allows
// it.
func (p *sprayPlan) requestURL(cred Credential) string {
	if p.method != http.MethodGet {
		return p.url
	}
	parsed, err := url.Parse(p.url)
	if err != nil {
		return p.url
	}
	query := parsed.Query()
	query.Set(p.usernameField, cred.Username)
	query.Set(p.passwordField, cred.Password)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// request builds the request one credential is tried with.
func (p *sprayPlan) request(cred Credential) recon.Request {
	req := recon.Request{
		URL:     p.requestURL(cred),
		Method:  p.method,
		Headers: p.headers,
		Timeout: p.timeout,
	}
	if p.method != http.MethodGet {
		req.Body = p.body.render(cred.Username, cred.Password)
	}
	return req
}

// ---------------------------------------------------------------------------
// The spray
// ---------------------------------------------------------------------------

// sprayState is the accumulator of a spray. Several workers write to it, so
// every field sits behind the mutex.
type sprayState struct {
	mu            sync.Mutex
	outcomes      []Outcome
	attempts      map[string]int
	stopped       map[string]bool
	authenticated map[string]bool
	usedNames     map[string]bool
	failures      map[string]int
	sessions      []string
	errors        []string
	refused       int
	stopReason    string
	// maxPerUser is the ceiling on the attempts one account gets.
	maxPerUser int
}

// attempt performs one gated, budgeted, receipted attempt and records what
// came back.
func (s *sprayState) attempt(ctx context.Context, cred Credential, plan *sprayPlan) {
	if !s.reserve(cred.Username) {
		return
	}
	// The gate and the budget are charged before the request exists, so a
	// refused attempt never leaves the machine. Both are the checks a probe
	// makes: the scope decides whether the destination may be contacted at
	// all, and the budget meters what was sent.
	if err := recon.Gate(plan.url); err != nil {
		s.mu.Lock()
		s.refused++
		s.addErrorLocked(fmt.Sprintf("%s refused by scope: %v", cred.Username, err))
		s.mu.Unlock()
		// Every attempt goes to the same endpoint, so one refusal decides
		// the rest of them.
		s.setStopped(fmt.Sprintf("refused by scope: %v", err))
		return
	}
	if err := recon.Take(1); err != nil {
		s.setStopped(err.Error())
		return
	}
	if plan.delay > 0 {
		s.waitBeforeAttempt(plan.delay)
	}

	req := plan.request(cred)

	attempt, err := plan.probe(ctx, req, plan.bodyCap)
	if err != nil {
		s.mu.Lock()
		s.addErrorLocked(fmt.Sprintf("%s: %v", cred.Username, err))
		s.mu.Unlock()
		return
	}

	success, failure, evidence := plan.classify(attempt)
	outcome := Outcome{
		Credential: cred,
		Status:     attempt.Result.Status,
		Success:    success,
		Failure:    failure,
		Evidence:   scrub(evidence, cred.Password),
		ElapsedMS:  attempt.Result.ElapsedMS,
		SHA256:     attempt.Result.SHA256,
		ReceiptID:  attempt.Result.ReceiptID,
	}
	if success {
		outcome.Failure = ""
	}

	s.mu.Lock()
	s.outcomes = append(s.outcomes, outcome)
	if success {
		s.authenticated[cred.Username] = true
	} else if failure != "" {
		s.failures[failure]++
	}
	s.mu.Unlock()

	if success {
		s.storeSession(ctx, cred, plan, attempt)
	}
}

// reserve books one attempt against the per-user ceiling.
//
// The ceiling is what keeps a spray from locking the accounts it is testing:
// once an account has had its attempts, no more go out for it, and the
// account is counted so the summary can say how many were cut off.
func (s *sprayState) reserve(username string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.authenticated[username] {
		// The account is already in. Trying more passwords against it is
		// noise, and noise on a working account is what triggers a lock.
		return false
	}
	if s.maxPerUser > 0 && s.attempts[username] >= s.maxPerUser {
		if !s.stopped[username] {
			s.stopped[username] = true
		}
		return false
	}
	s.attempts[username]++
	return true
}

// waitBeforeAttempt pauses between attempts.
//
// The pause is what a lockout counter sees as separate attempts, so it is
// applied before the request rather than after it: the first attempt of a
// spray waits too, because the spray may be following another one.
func (s *sprayState) waitBeforeAttempt(delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	<-timer.C
}

// storeSession keeps the session a successful login produced.
//
// The point of a successful credential is the session that follows it, so the
// cookies of the response and the request headers worth replaying are stored
// under a name the caller can hand to auth_replay.
func (s *sprayState) storeSession(ctx context.Context, cred Credential, plan *sprayPlan, attempt Attempt) {
	if plan.store == nil {
		return
	}
	cookies := attempt.Cookies
	if len(cookies) == 0 {
		// The probe records that a cookie was set without recording its
		// value, so the session is captured with one extra authenticated
		// request. It is gated and budgeted like every other request.
		captured, err := plan.capture(ctx, plan.request(cred))
		if err != nil {
			s.mu.Lock()
			s.addErrorLocked(fmt.Sprintf("session capture for %s: %v", cred.Username, err))
			s.mu.Unlock()
		} else {
			cookies = captured
		}
	}

	name := s.reserveSessionName(plan.sessionBase, cred.Username)
	sess := jar.Session{
		Name:    name,
		Cookies: cookies,
		Headers: replayableHeaders(plan.headers, tokenFromBody(attempt.Body)),
		Meta: map[string]string{
			"login_url":    plan.url,
			"login_method": plan.method,
			"username":     cred.Username,
			"source":       cred.Source,
			"logged_in_at": time.Now().UTC().Format(time.RFC3339),
			"login_status": fmt.Sprintf("%d", attempt.Result.Status),
		},
	}
	if err := plan.store.Save(sess); err != nil {
		s.mu.Lock()
		s.addErrorLocked(fmt.Sprintf("store session %q: %v", name, err))
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.sessions = append(s.sessions, name)
	s.mu.Unlock()
}

// reserveSessionName picks the name a session is stored under.
//
// A spray can authenticate more than one account and a session store holds
// one account per name, so the second success takes the name with the
// username appended rather than overwriting the first.
func (s *sprayState) reserveSessionName(base, username string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := sanitizeName(base)
	if name == "" {
		name = sanitizeName("cred-" + username)
	}
	if s.usedNames[name] {
		name = sanitizeName(name + "-" + username)
	}
	s.usedNames[name] = true
	return name
}

// outcomesSnapshot copies the outcomes out.
func (s *sprayState) outcomesSnapshot() []Outcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Outcome, len(s.outcomes))
	copy(out, s.outcomes)
	return out
}

// summarySnapshot copies the counters out.
func (s *sprayState) summarySnapshot() Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	failures := make(map[string]int, len(s.failures))
	for reason, count := range s.failures {
		failures[reason] = count
	}
	errors := make([]string, len(s.errors))
	copy(errors, s.errors)
	sessions := make([]string, len(s.sessions))
	copy(sessions, s.sessions)
	stopped := 0
	for _, was := range s.stopped {
		if was {
			stopped++
		}
	}
	summary := Summary{
		Attempts:     len(s.outcomes),
		Successes:    len(s.authenticated),
		Failures:     failures,
		UsersStopped: stopped,
		Refused:      s.refused,
		Errors:       errors,
		Sessions:     sessions,
	}
	if strings.Contains(strings.ToLower(s.stopReason), "rate limit") ||
		strings.Contains(strings.ToLower(s.stopReason), "budget exhausted") {
		summary.RateLimited = true
		summary.RateLimit = s.stopReason
	}
	return summary
}

// addErrorLocked records a bounded number of errors.
func (s *sprayState) addErrorLocked(message string) {
	if len(s.errors) >= maxReportedErrors {
		return
	}
	s.errors = append(s.errors, message)
}

// setStopped records why the spray ended early.
func (s *sprayState) setStopped(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopReason == "" {
		s.stopReason = reason
	}
}

// isStopped reports whether the spray has to stop.
func (s *sprayState) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopReason != ""
}

// ---------------------------------------------------------------------------
// Classification
// ---------------------------------------------------------------------------

// classify decides what an attempt produced: whether the credentials worked,
// why they did not, and the evidence around the signal.
func (p *sprayPlan) classify(attempt Attempt) (bool, string, string) {
	text := string(attempt.Body)
	lower := strings.ToLower(text)

	if p.successMarker != "" {
		marker := strings.ToLower(p.successMarker)
		if index := strings.Index(lower, marker); index >= 0 {
			return true, "", excerpt(text, index, len(marker))
		}
		// The marker was configured and did not appear. That is the answer:
		// the target did not say the login worked.
		reason, evidence := p.reasonFor(attempt, text)
		return false, reason, evidence
	}

	if p.successField != "" {
		value, ok := jsonField(attempt.Body, p.successField)
		if ok && successValue(value) {
			return true, "", clipValue(value)
		}
		reason, evidence := p.reasonFor(attempt, text)
		return false, reason, evidence
	}

	// Nothing configured: a 2xx is the success.
	if attempt.Result.Status >= 200 && attempt.Result.Status < 300 {
		return true, "", ""
	}
	reason, evidence := p.reasonFor(attempt, text)
	return false, reason, evidence
}

// reasonFor names why an attempt failed.
//
// The markers come before the status, because a locked account often answers
// 200 with a message that says so, and a status alone would report it as a
// wrong password.
func (p *sprayPlan) reasonFor(attempt Attempt, text string) (string, string) {
	if marker, index, ok := markerMatch(text, p.lockedMarkers); ok {
		return FailureLocked, excerpt(text, index, len(marker))
	}
	if marker, index, ok := markerMatch(text, p.failureMarkers); ok {
		return failureFromMarker(marker), excerpt(text, index, len(marker))
	}
	if attempt.Result.Status == p.rateLimitStatus {
		return FailureRateLimited, ""
	}
	if attempt.Result.Status >= 500 {
		return FailureServerError, ""
	}
	return FailureWrongPassword, ""
}

// failureFromMarker names the reason a failure marker stands for.
//
// A captcha and a rate limit are not a wrong password, and a report that
// calls them one sends the reader after the wrong thing.
func failureFromMarker(marker string) string {
	lower := strings.ToLower(marker)
	switch {
	case strings.Contains(lower, "captcha"):
		return FailureCaptcha
	case strings.Contains(lower, "too many"), strings.Contains(lower, "rate limit"), strings.Contains(lower, "ratelimit"):
		return FailureRateLimited
	default:
		return FailureWrongPassword
	}
}

// markerMatch finds the first marker that appears in the text.
func markerMatch(text string, markers []string) (string, int, bool) {
	lower := strings.ToLower(text)
	for _, marker := range markers {
		index := strings.Index(lower, strings.ToLower(marker))
		if index >= 0 {
			return marker, index, true
		}
	}
	return "", 0, false
}

// jsonField reads a dotted path out of a JSON document.
func jsonField(body []byte, path string) (any, bool) {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, false
	}
	current := doc
	for _, part := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

// successValue decides what a JSON field's value says.
//
// A boolean is the obvious case. A string is the common one, because a login
// API answers {"status":"ok"} as often as it answers {"authenticated":true}.
func successValue(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case float64:
		return typed != 0
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "ok", "okay", "success", "successful", "authenticated", "logged_in", "loggedin", "yes", "valid", "1":
			return true
		}
		return false
	default:
		return false
	}
}

// clipValue renders a JSON value for the evidence, without carrying a token
// into the report.
func clipValue(value any) string {
	switch typed := value.(type) {
	case bool:
		return fmt.Sprintf("%t", typed)
	case float64:
		return fmt.Sprintf("%v", typed)
	case string:
		if len([]rune(typed)) > maxEvidenceRunes {
			return string([]rune(typed)[:maxEvidenceRunes]) + "..."
		}
		return typed
	default:
		return ""
	}
}

// excerpt returns a bounded piece of the body around a signal.
func excerpt(text string, byteIndex int, length int) string {
	runes := []rune(text)
	head := len([]rune(text[:byteIndex]))
	start := head - evidenceContextRunes
	if start < 0 {
		start = 0
	}
	end := head + length + evidenceContextRunes
	if end > len(runes) {
		end = len(runes)
	}
	out := strings.Join(strings.Fields(string(runes[start:end])), " ")
	if len([]rune(out)) > maxEvidenceRunes {
		out = string([]rune(out)[:maxEvidenceRunes]) + "..."
	}
	return out
}

// scrub removes the password from a piece of text.
//
// A target that echoes the submitted password back in its error page would
// otherwise put the credential in the report.
func scrub(text, password string) string {
	if password == "" || text == "" {
		return text
	}
	text = strings.ReplaceAll(text, password, "***")
	if escaped := url.QueryEscape(password); escaped != password {
		text = strings.ReplaceAll(text, escaped, "***")
	}
	return text
}

// ---------------------------------------------------------------------------
// The session
// ---------------------------------------------------------------------------

// replayHeaders are the request headers worth keeping with a session.
//
// Everything kept here is written to disk and sent again without being
// asked, so the list is short on purpose.
var replayHeaders = []string{
	"authorization",
	"x-api-key",
	"x-auth-token",
	"x-csrf-token",
	"x-xsrf-token",
}

// tokenFields are the JSON fields a login response carries its token in.
//
// A JSON API often authenticates with a token in the body rather than with
// a cookie, and a session that kept the token is a session that can be
// replayed.
var tokenFields = []string{"token", "access_token", "id_token", "jwt", "session_token", "api_key", "apiKey"}

// replayableHeaders picks the headers worth keeping out of a login's request
// headers, and adds the bearer token the response carried when there was
// one.
//
// A User-Agent or a Content-Type would be wrong to replay, and a Cookie
// belongs to the jar, not here.
func replayableHeaders(headers map[string]string, token string) map[string]string {
	out := make(map[string]string, len(replayHeaders)+1)
	for name, value := range headers {
		lower := strings.ToLower(name)
		for _, keep := range replayHeaders {
			if lower == keep {
				out[name] = value
				break
			}
		}
	}
	if token != "" {
		out["Authorization"] = "Bearer " + token
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// tokenFromBody reads the token a JSON login response carried.
func tokenFromBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	for _, field := range tokenFields {
		value, ok := jsonField(body, field)
		if !ok {
			continue
		}
		text, ok := value.(string)
		if !ok {
			continue
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		return text
	}
	return ""
}

// sanitizeName turns a username or a base name into a session name the jar
// accepts.
//
// The jar's name rules are deliberately narrow, and a username is not one of
// its inputs, so the characters it refuses are replaced rather than the
// session refused.
func sanitizeName(raw string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, strings.TrimSpace(raw))
	cleaned = strings.Trim(cleaned, ".")
	if len(cleaned) > maxSessionNameLen {
		cleaned = cleaned[:maxSessionNameLen]
	}
	return cleaned
}

// ---------------------------------------------------------------------------
// The real request seam
// ---------------------------------------------------------------------------

// defaultProbe is the real attempt: a bounded, receipted probe.
func defaultProbe(ctx context.Context, req recon.Request, bodyCap int) (Attempt, error) {
	res, body, err := recon.ProbeCapture(ctx, req, bodyCap)
	if err != nil {
		return Attempt{}, err
	}
	return Attempt{Result: res, Body: body}, nil
}

// defaultCapture authenticates once more and returns the cookies the
// response set.
//
// The probe deliberately does not carry cookie values: a receipt is
// evidence, not a warehouse. A successful login still has to become a
// usable session, so the spray performs this one extra authenticated
// request, through the same guarded transport and under the same gate and
// budget as the attempt itself.
func defaultCapture(ctx context.Context, req recon.Request) ([]jar.Cookie, error) {
	if err := recon.Gate(req.URL); err != nil {
		return nil, fmt.Errorf("refused, not sent: %w", err)
	}
	if err := recon.Take(1); err != nil {
		return nil, err
	}
	parsed, err := url.Parse(req.URL)
	if err != nil {
		return nil, fmt.Errorf("url is not valid: %w", err)
	}
	jarClient, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create the cookie jar: %w", err)
	}
	client := &http.Client{
		Transport: netguard.Transport(),
		Jar:       jarClient,
		// A redirect is a result, not a hop: the gate applied to the URL
		// that was asked for, and following a 3xx would move the request
		// to a host nobody authorized.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	var body io.Reader
	if req.Body != "" {
		body = strings.NewReader(req.Body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, parsed.String(), body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	for name, value := range req.Headers {
		httpReq.Header.Set(name, value)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("session capture request failed: %w", err)
	}
	defer resp.Body.Close()
	return cookiesFromResponse(resp, parsed), nil
}

// cookiesFromResponse reads the cookies a response set, under the rules a
// browser applies.
//
// A cookie whose domain does not cover the host that set it is not a session
// cookie, a cookie that is being deleted is not stored at all, and control
// characters are stripped from a name and a value before either can be
// replayed into a request header.
func cookiesFromResponse(resp *http.Response, u *url.URL) []jar.Cookie {
	host := strings.ToLower(u.Hostname())
	now := time.Now()
	out := make([]jar.Cookie, 0, len(resp.Cookies()))
	for _, c := range resp.Cookies() {
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
		out = append(out, jar.Cookie{
			Name:     name,
			Value:    cookieText(c.Value),
			Domain:   domain,
			Path:     path,
			Expires:  expires,
			Secure:   c.Secure,
			HTTPOnly: c.HttpOnly,
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

// defaultCookiePath is the path a cookie without a Path attribute covers.
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
func cookieText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
