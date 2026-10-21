package recon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"ceraa/harness/netguard"
)

// stubFetcher stands in for assetFetcher during tests: every response is
// loaded up front and the URLs actually asked for are recorded, so a test can
// prove a fetch did or did not happen. No test in this file dials anything.
type stubFetcher struct {
	requested []string
	bodies    map[string]stubBody
}

type stubBody struct {
	body []byte
	ct   string
}

func (s *stubFetcher) fetch(ctx context.Context, target string) ([]byte, string, error) {
	s.requested = append(s.requested, target)
	b, ok := s.bodies[target]
	if !ok {
		return nil, "", fmt.Errorf("stubFetcher: unexpected fetch %s", target)
	}
	return b.body, b.ct, nil
}

// stubOut replaces the production fetcher with s and restores it when the
// test ends.
func stubOut(t *testing.T, s *stubFetcher) {
	t.Helper()
	previous := assetFetcher
	assetFetcher = s.fetch
	t.Cleanup(func() { assetFetcher = previous })
}

// TestSecretsScanMatchesOneExamplePerClass: a table of one body per class.
// Exact rows must land as exact, weak rows as weak, and a plain base64 blob
// that happens to be long and token-ish must not be claimed as any vendor.
func TestSecretsScanMatchesOneExamplePerClass(t *testing.T) {
	googleKey := "AIzaSy" + strings.Repeat("A", 33)
	slackToken := "xoxb-" + strings.Repeat("9", 14)
	awsKeyID := "AKIADQ3PL4566WJ7MR72"
	githubPAT := "ghp_" + strings.Repeat("f", 36)
	githubFine := "github_pat_" + strings.Repeat("w", 22) + "_" + strings.Repeat("v", 59)
	sendgridKey := "SG." + strings.Repeat("b", 22) + "." + strings.Repeat("c", 43)
	mailgunKey := "key-" + strings.Repeat("m", 32)
	openaiKey := "sk-" + strings.Repeat("O", 24)
	anthropicKey := "sk-ant-" + strings.Repeat("n", 24)
	googleClientID := "424242-abcdefghijklmnopqrstuvwxyz.apps.googleusercontent.com"
	pemHeader := "text before\n-----BEGIN RSA PRIVATE KEY-----\nmore"
	configLine := "password: \"hunter2utherford\""
	herokuID := "app heroku-12345 released 1b3d5f79-9a0b-4c2d-8e1f-a0b1c2d3e4f5 ok"
	bearerLine := "Authorization: bearer " + strings.Repeat("t", 30)
	jwtish := "eyJhbGciOiJIUzI1N.PreFunicularBoarding.singATUREZ"
	dbURL := "postgres://deploy:hunterOpener44@db.internal/Customers"
	queryKey := "https://maps.example/load?api_key=" + strings.Repeat("K", 24)
	webhookPath := "https://hooks.slack.com/services/AB12CD34EF/GH56IJ78KL"
	// Long, dense and token-like, yet shaped like no vendor's identifier and
	// shaped like none of the weak families either.
	innocentBase64 := strings.Repeat("VGhpc0lzTm90QVZlbmRvclBhdA==", 3)

	rows := []struct {
		name       string
		input      string
		pattern    string
		confidence string
		// alsoExact names other exact patterns whose shape legitimately
		// fires on this same body - the OpenAI sk- shape cannot tell an
		// sk-ant- value from its own, so both report.
		alsoExact []string
	}{
		{"google api key", googleKey, "Google API key", ConfidenceExact, nil},
		{"slack token", slackToken, "Slack token", ConfidenceExact, nil},
		{"aws access key id", awsKeyID, "AWS access key id", ConfidenceExact, nil},
		{"github classic pat", githubPAT, "GitHub personal access token", ConfidenceExact, nil},
		{"github fine-grained pat", githubFine, "GitHub fine-grained PAT", ConfidenceExact, nil},
		{"sendgrid key", sendgridKey, "SendGrid API key", ConfidenceExact, nil},
		{"mailgun key", mailgunKey, "Mailgun API key", ConfidenceExact, nil},
		{"openai-style key", openaiKey, "OpenAI-style API key", ConfidenceExact, nil},
		{"anthropic key", anthropicKey, "Anthropic API key", ConfidenceExact, []string{"OpenAI-style API key"}},
		{"google oauth client id", googleClientID, "Google OAuth client id", ConfidenceExact, nil},
		{"private key header", pemHeader, "Private key header", ConfidenceExact, nil},
		{"yaml password line", configLine, "Credential-style config line", ConfidenceWeak, nil},
		{"heroku id", herokuID, "Heroku-style id", ConfidenceWeak, nil},
		{"bearer line", bearerLine, "Bearer token in text", ConfidenceWeak, nil},
		{"jwt-looking string", jwtish, "JWT-looking string", ConfidenceWeak, nil},
		{"database url with password", dbURL, "Database URL with password", ConfidenceWeak, nil},
		{"api key in query string", queryKey, "API key in query string", ConfidenceWeak, nil},
		{"slack webhook path", webhookPath, "Slack webhook path", ConfidenceWeak, nil},
	}
	for _, row := range rows {
		findings := Scan([]byte("noise\n" + row.input + "\nnoise\n"))
		matched := false
		for _, f := range findings {
			if f.Name != row.pattern {
				if f.Confidence == ConfidenceExact && !contains(row.alsoExact, f.Name) {
					t.Errorf("%s: unexpected exact hit %q", row.name, f.Name)
				}
				continue
			}
			matched = true
			if f.Confidence != row.confidence {
				t.Errorf("%s: %q reported %q, want %q", row.name, row.pattern, f.Confidence, row.confidence)
			}
			if f.Masked == "" {
				t.Errorf("%s: finding carries an empty receipt", row.name)
			}
		}
		if !matched {
			t.Errorf("%s: pattern %q missed body %q", row.name, row.pattern, row.input)
		}
	}

	// false-positive guard: nothing token-shaped but not vendor-shaped
	for _, f := range Scan([]byte(innocentBase64)) {
		t.Errorf("innocent base64 matched %q (%s) and should have matched nothing", f.Name, f.Masked)
	}

	// whole-body sweep: every class at once, every confidence accounted for.
	// The one tolerated extra exact receipt is the OpenAI shape firing on the
	// sk-ant- value, which is a second report of the same string, not a new
	// secret.
	var sb strings.Builder
	for _, row := range rows {
		sb.WriteString(row.input)
		sb.WriteString("\n")
	}
	sb.WriteString(innocentBase64)
	swept := Scan([]byte(sb.String()))
	exactRows := 0
	weakRows := 0
	for _, row := range rows {
		if row.confidence == ConfidenceExact {
			exactRows++
		} else {
			weakRows++
		}
	}
	exactCount := 0
	weakCount := 0
	for _, f := range swept {
		switch f.Confidence {
		case ConfidenceExact:
			exactCount++
		case ConfidenceWeak:
			weakCount++
		default:
			t.Errorf("finding %q has unknown confidence %q", f.Name, f.Confidence)
		}
	}
	if exactCount < exactRows {
		t.Errorf("exact findings over the combined body = %d, want at least one per exact row (%d)", exactCount, exactRows)
	}
	if exactCount > exactRows+1 {
		t.Errorf("exact findings over the combined body = %d, want %d rows plus at most the sk-ant overlap", exactCount, exactRows)
	}
	if weakRows > 0 && weakCount == 0 {
		t.Error("combined body produced no weak findings")
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// TestSecretsScanMaskIsReceiptOnly pins the receipt format: first six
// characters, a literal ***, the byte length, and nothing else.
func TestSecretsScanMaskIsReceiptOnly(t *testing.T) {
	value := "AKIADQ3PL4566WJ7MR72"
	findings := Scan([]byte("k=" + value + ";"))
	if len(findings) == 0 {
		t.Fatal("aws key not matched")
	}
	f := findings[0]
	want := fmt.Sprintf("AKIADQ*** (%d chars)", len(value))
	if f.Masked != want {
		t.Errorf("Masked = %q, want %q", f.Masked, want)
	}
	if strings.Contains(f.Masked, "PL45") {
		t.Errorf("Masked carries content beyond the receipt: %q", f.Masked)
	}
	if f.ByteOffset != 2 {
		t.Errorf("ByteOffset = %d, want 2 (after \"k=\")", f.ByteOffset)
	}
}

// TestSecretsScanCapsAtForty: fifty distinct keys of one class must never
// produce more than maxFindings findings. The keys differ inside their first
// six characters, so this is a cap test and not a dedupe test.
func TestSecretsScanCapsAtForty(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 50; i++ {
		sb.WriteString(fmt.Sprintf("AIza%02d%s\n", i, strings.Repeat("q", 33)))
	}
	findings := Scan([]byte(sb.String()))
	if len(findings) != maxFindings {
		t.Errorf("%d findings from 50 distinct keys, want the cap of %d", len(findings), maxFindings)
	}
}

// TestSecretsScanDedupesRepeatedMatches: the same string on many lines is one
// receipt, not twenty.
func TestSecretsScanDedupesRepeatedMatches(t *testing.T) {
	one := "key-" + strings.Repeat("d", 32)
	twenty := strings.Join([]string{one, one, one, one}, "\n")
	findings := Scan([]byte(twenty))
	if len(findings) != 1 {
		t.Errorf("%d findings from four identical matches, want 1", len(findings))
	}
}

// --- ScanAssets: every test stubs assetFetcher, so nothing here dials ---

// TestSecretsScanAssetsPrefersSameOriginUnderCap: same-origin assets are
// collected first, cross origin last, and past the cap the remaining
// candidates are never even fetched.
func TestSecretsScanAssetsPrefersSameOriginUnderCap(t *testing.T) {
	page := "https://site.test/index.html"
	first := "https://site.test/static/one.js"
	cdn := "https://cdn.other/vendor.js"
	second := "https://site.test/static/two.js"
	third := "https://site.test/static/three.js"
	pageHTML := fmt.Sprintf(`<html><script src="/static/one.js"></script>
<script src="%s"></script>
<script src="/static/two.js"></script>
<script src="/static/three.js"></script></html>`, cdn)
	stub := &stubFetcher{bodies: map[string]stubBody{
		page:   {body: []byte(pageHTML), ct: "text/html; charset=utf-8"},
		first:  {body: []byte("// one"), ct: "application/javascript"},
		cdn:    {body: []byte("// vendor code, no markers"), ct: "application/javascript"},
		second: {body: []byte("// two"), ct: "application/javascript"},
		third:  {body: []byte("// three"), ct: "application/javascript"},
	}}
	stubOut(t, stub)

	findings, err := ScanAssets(context.Background(), page, 2)
	if err != nil {
		t.Fatalf("ScanAssets: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("clean bodies gave %d findings, want 0: %v", len(findings), findings)
	}
	got := strings.Join(stub.requested, " | ")
	want := page + " | " + first + " | " + second
	if got != want {
		t.Errorf("fetch order = %q, want %q", got, want)
	}
}

// TestSecretsScanAssetsScansPageAndAssets: each finding attributes where it
// was seen, page for the page and the fetch URL for the scripts.
func TestSecretsScanAssetsScansPageAndAssets(t *testing.T) {
	page := "https://site.test/app.html"
	asset := "https://site.test/bundle.js"
	pageHTML := `<html><script src="/bundle.js"></script>
<script>var cfc = "AKIADQ3PL4566WJ7MR72";</script></html>`
	stub := &stubFetcher{bodies: map[string]stubBody{
		page: {body: []byte(pageHTML), ct: "text/html"},
		asset: {body: []byte("const a=1;\nconst k=\"SG." + strings.Repeat("X", 22) +
			"." + strings.Repeat("Y", 43) + "\";"), ct: "application/javascript"},
	}}
	stubOut(t, stub)

	findings, err := ScanAssets(context.Background(), page, 0)
	if err != nil {
		t.Fatalf("ScanAssets: %v", err)
	}
	var pageNames, assetNames []string
	for _, f := range findings {
		switch f.Asset {
		case page:
			pageNames = append(pageNames, f.Name)
		case asset:
			assetNames = append(assetNames, f.Name)
		default:
			t.Errorf("finding on unexpected asset %q: %q", f.Asset, f.Name)
		}
	}
	if len(pageNames) != 1 || pageNames[0] != "AWS access key id" {
		t.Errorf("page findings = %v, want one AWS access key id", pageNames)
	}
	if len(assetNames) != 1 || assetNames[0] != "SendGrid API key" {
		t.Errorf("asset findings = %v, want one SendGrid API key", assetNames)
	}
}

// TestSecretsScanAssetsFetchesSourceMapOnlyWhenReferenced: the .map is
// requested only when the script body itself carries a sourceMappingURL
// marker, and the map's findings carry the map's URL.
func TestSecretsScanAssetsFetchesSourceMapOnlyWhenReferenced(t *testing.T) {
	paged := "https://site.test/p.html"
	marked := "https://site.test/marked.js"
	plain := "https://site.test/plain.js"
	pageHTML := `<html><script src="marked.js"></script><script src="plain.js"></script></html>`
	stub := &stubFetcher{bodies: map[string]stubBody{
		paged:           {body: []byte(pageHTML), ct: "text/html"},
		marked:          {body: []byte("//# sourceMappingURL=marked.js.map"), ct: "application/javascript"},
		marked + ".map": {body: []byte("config value: bearer " + strings.Repeat("m", 28)), ct: "application/json"},
		plain:           {body: []byte("// nothing declared here"), ct: "application/javascript"},
	}}
	stubOut(t, stub)

	findings, err := ScanAssets(context.Background(), paged, 0)
	if err != nil {
		t.Fatalf("ScanAssets: %v", err)
	}
	fetched := strings.Join(stub.requested, " | ")
	if !strings.Contains(fetched, marked+".map") {
		t.Errorf("source map not fetched though the script declared one; fetched: %s", fetched)
	}
	sawMapFinding := false
	for _, f := range findings {
		if f.Asset != marked+".map" {
			continue
		}
		sawMapFinding = true
		if f.Name != "Bearer token in text" || f.Confidence != ConfidenceWeak {
			t.Errorf("map finding = %q (%s), want weak bearer", f.Name, f.Confidence)
		}
	}
	if !sawMapFinding {
		t.Errorf("no finding attributed to %s; got %v", marked+".map", findings)
	}
	for _, req := range stub.requested {
		if req == plain+".map" {
			t.Errorf("map fetched for an asset that never declared a sourceMappingURL")
		}
	}
}

// TestSecretsScanAssetsSkipsBinaryBodies: an image body is fetched and then
// never scanned.
func TestSecretsScanAssetsSkipsBinaryBodies(t *testing.T) {
	page := "https://site.test/i.html"
	img := "https://site.test/logo.png"
	stub := &stubFetcher{bodies: map[string]stubBody{
		page: {body: []byte(`<html><script src="logo.png"></script></html>`), ct: "text/html"},
		img:  {body: []byte("AKIAIMGNOTREALX77"), ct: "image/png"},
	}}
	stubOut(t, stub)

	findings, err := ScanAssets(context.Background(), page, 0)
	if err != nil {
		t.Fatalf("ScanAssets: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("binary body yielded %v, want nothing scanned", findings)
	}
	if len(stub.requested) != 2 {
		t.Errorf("requested = %v, want one page and one asset attempt", stub.requested)
	}
}

// TestSecretsScanAssetsRefusesLoopbackPage: the page itself passes the guard
// before anything is fetched, and the refusal is netguard's.
func TestSecretsScanAssetsRefusesLoopbackPage(t *testing.T) {
	stub := &stubFetcher{bodies: map[string]stubBody{}}
	stubOut(t, stub)

	_, err := ScanAssets(context.Background(), "http://127.0.0.1:8787/index.html", 0)
	if err == nil {
		t.Fatal("loopback page accepted, want netguard refusal")
	}
	if !errors.Is(err, netguard.ErrBlocked) {
		t.Errorf("error = %v, want ErrBlocked", err)
	}
	if len(stub.requested) != 0 {
		t.Errorf("fetcher called on a refused page: %v", stub.requested)
	}
}

// TestSecretsScanAssetsRejectsNonHTTPScheme: the scheme guard sits at the
// ScanAssets entry, so a file: page never reaches the fetcher.
func TestSecretsScanAssetsRejectsNonHTTPScheme(t *testing.T) {
	stub := &stubFetcher{bodies: map[string]stubBody{}}
	stubOut(t, stub)

	if _, err := ScanAssets(context.Background(), "file:///etc/hosts", 0); err == nil {
		t.Fatal("file: scheme accepted")
	}
	if len(stub.requested) != 0 {
		t.Errorf("fetcher called: %v", stub.requested)
	}
}

// TestSecretsScanAssetsClampsToHardCap: a caller cannot raise the fetch
// count past the hard cap no matter what the args ask for.
func TestSecretsScanAssetsClampsToHardCap(t *testing.T) {
	page := "https://site.test/p.html"
	var sb strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&sb, `<script src="/a%d.js"></script>`, i)
	}
	stub := &stubFetcher{bodies: map[string]stubBody{page: {body: []byte(sb.String()), ct: "text/html"}}}
	for i := 0; i < 40; i++ {
		stub.bodies[fmt.Sprintf("https://site.test/a%d.js", i)] = stubBody{body: []byte("// blank"), ct: "application/javascript"}
	}
	stubOut(t, stub)

	if _, err := ScanAssets(context.Background(), page, 5000); err != nil {
		t.Fatalf("ScanAssets: %v", err)
	}
	if n := len(stub.requested); n != hardAssetCap+1 { // page plus the asset cap
		t.Errorf("fetched %d urls, want %d (page plus hard cap)", n, hardAssetCap+1)
	}
}
