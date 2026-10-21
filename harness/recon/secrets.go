// Secret scanning for recon pages and assets.
//
// The scanner works through a pattern table, not a tool chain: every body that
// Scan looks at is run against a fixed list of regular expressions, and what
// comes out is a list of masked receipts. Nothing is validated against a
// vendor, nothing is tested for liveness, and no full secret value ever exists
// in a finding. A match's job here is to appear in the operator's report with
// enough of itself visible to relocate it in the source, and no more.
//
// Every pattern below is written fresh from public knowledge of what the
// vendors' identifiers look like. Nothing here is derived from, imported from
// or mechanically transformed out of any third-party scanner source, and the
// list is meant to be read and extended in this file, not imported.
package recon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"ceraa/harness/netguard"
)

// Confidence marks how much a match deserves to be trusted. "exact" is for
// vendor-shaped identifiers whose structure only the vendor's own key space
// has: prefix plus checksummed or fixed-length body. "weak" is for shapes a
// curious person might write down in a yaml file or a header, which are leads,
// not confirmations.
const (
	ConfidenceExact = "exact"
	ConfidenceWeak  = "weak"
)

// Finding is one mask over one match. Masked is a receipt for the report, not
// a secret: it carries the first six characters so the match can be relocated
// in the source, and the total length, and nothing else. The full value stays
// in the body that was scanned and is written nowhere.
type Finding struct {
	Name       string
	Confidence string // "exact" or "weak"
	Masked     string
	ByteOffset int
}

// Pattern is one entry in the scanner's table.
type Pattern struct {
	Name       string
	Re         *regexp.Regexp
	Confidence string
}

// Bounds. A body produced by a hostile server must not be able to make Scan
// allocate or loop unboundedly, and a page with a wall of candidate strings
// yields a bounded list of findings, not a transcript.
const (
	maxFindings      = 40
	defaultAssetCap  = 12
	hardAssetCap     = 30
	maxPageBytes     = 2 * 1024 * 1024
	maxAssetBytes    = 1 * 1024 * 1024
	maxScanWallTime  = 60 * time.Second
	perRequestBound  = 30 * time.Second
	requestChunkSize = 64 * 1024
)

// Patterns returns the table the scanner runs. Vendor-shaped identifiers are
// "exact"; shapes that merely look like credentials are "weak".
func Patterns() []Pattern {
	return []Pattern{
		// --- exact: vendor-shaped ids ---

		{Name: "Google API key", Confidence: ConfidenceExact,
			Re: regexp.MustCompile(`AIza[0-9A-Za-z_\-]{35}`)},
		{Name: "Slack token", Confidence: ConfidenceExact,
			Re: regexp.MustCompile(`xox[bpars]-[\w-]{10,}`)},
		{Name: "AWS access key id", Confidence: ConfidenceExact,
			Re: regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
		{Name: "GitHub personal access token", Confidence: ConfidenceExact,
			Re: regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`)},
		{Name: "GitHub OAuth token", Confidence: ConfidenceExact,
			Re: regexp.MustCompile(`gho_[A-Za-z0-9]{36}`)},
		{Name: "GitHub fine-grained PAT", Confidence: ConfidenceExact,
			Re: regexp.MustCompile(`github_pat_[\w]{22}_[\w]{59}`)},
		{Name: "SendGrid API key", Confidence: ConfidenceExact,
			Re: regexp.MustCompile(`SG\.[A-Za-z0-9_\-]{22}\.[A-Za-z0-9_\-]{43}`)},
		{Name: "Mailgun API key", Confidence: ConfidenceExact,
			Re: regexp.MustCompile(`key-[a-z0-9]{32}`)},
		// OpenAI-style keys carry no checksum, so the shape is technically a
		// guess; the sk- prefix followed by twenty-plus identifier characters
		// is still vendor-specific enough to report as exact. A key that
		// starts sk-ant- also matches the next entry below it, which reads as
		// one string found twice, and that is fine: the report says which
		// pattern saw it.
		{Name: "OpenAI-style API key", Confidence: ConfidenceExact,
			Re: regexp.MustCompile(`sk-[A-Za-z0-9_\-]{20,}`)},
		{Name: "Anthropic API key", Confidence: ConfidenceExact,
			Re: regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{20,}`)},
		{Name: "Google OAuth client id", Confidence: ConfidenceExact,
			Re: regexp.MustCompile(`\b[0-9]{1,25}-[a-z0-9_]{10,}\.apps\.googleusercontent\.com`)},
		{Name: "Private key header", Confidence: ConfidenceExact,
			Re: regexp.MustCompile(`-----BEGIN (RSA|EC|DSA|OPENSSH|PGP) PRIVATE KEY-----`)},

		// --- weak: shapes that look like credentials ---

		{Name: "Credential-style config line", Confidence: ConfidenceWeak,
			Re: regexp.MustCompile(`(?im)^\s*(?:password|passwd|pwd|secret|api[_-]?key|auth[_-]?token|access[_-]?token|private[_-]?key)\s*[:=]\s*["']?\S{8,}`)},
		{Name: "Heroku-style id", Confidence: ConfidenceWeak,
			Re: regexp.MustCompile(`(?i)heroku[^\n]{0,20}[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)},
		{Name: "Bearer token in text", Confidence: ConfidenceWeak,
			Re: regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9_\-\.=]{20,}`)},
		{Name: "JWT-looking string", Confidence: ConfidenceWeak,
			Re: regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`)},
		{Name: "Database URL with password", Confidence: ConfidenceWeak,
			Re: regexp.MustCompile(`(?i)\b(?:postgres(?:ql)?|mysql|mongodb(?:\+srv)?|amqp)://[^\s:/]{3,}:[^\s@]{6,}@`)},
		{Name: "API key in query string", Confidence: ConfidenceWeak,
			Re: regexp.MustCompile(`(?i)[?&](?:api_?key|access_?token|token)=[A-Za-z0-9_\-\.=]{16,}`)},
		{Name: "Slack webhook path", Confidence: ConfidenceWeak,
			Re: regexp.MustCompile(`hooks\.slack\.com/services/[A-Za-z0-9]{8,}/[A-Za-z0-9]{8,}`)},
	}
}

// Scan runs the pattern table over a body and returns at most maxFindings
// findings, deduplicated by pattern and masked value. Matches are receipts:
// Masked shows the first six characters and the length, never the value.
func Scan(body []byte) []Finding {
	findings := make([]Finding, 0, len(Patterns()))
	seen := make(map[string]bool)
	for _, pattern := range Patterns() {
		for _, loc := range pattern.Re.FindAllIndex(body, -1) {
			masked := Masked(body[loc[0]:loc[1]])
			key := pattern.Name + "\x00" + masked
			if seen[key] {
				continue
			}
			seen[key] = true
			findings = append(findings, Finding{
				Name:       pattern.Name,
				Confidence: pattern.Confidence,
				Masked:     masked,
				ByteOffset: loc[0],
			})
			if len(findings) == maxFindings {
				return findings
			}
		}
	}
	return findings
}

// Masked renders the receipt form: enough of the match to find it again
// (at most six characters), a literal "***", and the byte length. A match too
// short to survive the cut is "***" alone, so nothing recognizable escapes.
func Masked(match []byte) string {
	length := len(match)
	if length <= 6 {
		return fmt.Sprintf("*** (%d chars)", length)
	}
	return fmt.Sprintf("%s*** (%d chars)", match[:6], length)
}

// ---------------------------------------------------------------------------

// PageFinding is one Finding tied to where it was seen. For the page itself
// Asset is the page URL; for a script or map it is that fetch's URL.
type PageFinding struct {
	Asset      string
	Name       string
	Confidence string
	Masked     string
}

// assetFetcher fetches one URL and gives back the body and content type. The
// production implementation is guardedHTTPFetch, which dials only through
// netguard; tests replace the var so no test ever touches a socket.
var assetFetcher = guardedHTTPFetch

// scriptSrc and linkJsHref collect the script tags and link tags whose target
// is a .js file. Both are written for the job rather than borrowed: a page's
// asset list is a suggestion of what to scan, not a contract.
var (
	scriptSrcRe  = regexp.MustCompile(`(?i)<script[^>]*\bsrc\s*=\s*["']([^"']+)["']`)
	linkJSHrefRe = regexp.MustCompile(`(?i)<link[^>]*\bhref\s*=\s*["']([^"']*\.js)["']`)
)

// ScanAssets fetches pageurl, scans it, then follows the page's script and
// script-like link references and scans those bodies too, including a source
// map when a .js body itself announces it carries one.
//
// READ-ONLY GUARANTEE: every request here is a plain GET for content. Nothing
// is ever submitted to a status endpoint, tested for liveness, or probed with
// a finding's value attached - no request ever carries a secret that was
// found. Findings land in the returned slice, and from there only in the
// operator's own report.
//
// Limits: the page body is capped at 2MB, each asset body at 1MB, same-origin
// assets are preferred and the count is bounded by maxAssets (default 12,
// hard cap 30), and the whole run sits inside one 60s context.
func ScanAssets(ctx context.Context, pageurl string, maxAssets int) ([]PageFinding, error) {
	pageurl = strings.TrimSpace(pageurl)
	if pageurl == "" {
		return nil, errors.New("page url is required")
	}
	if !strings.HasPrefix(pageurl, "http://") && !strings.HasPrefix(pageurl, "https://") {
		return nil, fmt.Errorf("page url must use http:// or https://: %s", pageurl)
	}
	if maxAssets <= 0 {
		maxAssets = defaultAssetCap
	}
	if maxAssets > hardAssetCap {
		maxAssets = hardAssetCap
	}
	// The guard is checked here as well as in the fetcher so a refusal names
	// the address the model asked for rather than one of the assets behind it.
	if err := netguard.CheckURL(ctx, pageurl); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, maxScanWallTime)
	defer cancel()

	pageBody, contentType, err := assetFetcher(ctx, pageurl)
	if err != nil {
		return nil, fmt.Errorf("fetch page: %w", err)
	}

	var out []PageFinding
	if textEligible(contentType) {
		pageFindings := Scan(pageBody[:clampLen(len(pageBody), maxPageBytes)])
		for _, f := range pageFindings {
			out = append(out, PageFinding{Asset: pageurl, Name: f.Name, Confidence: f.Confidence, Masked: f.Masked})
		}
	}

	base, err := url.Parse(pageurl)
	if err != nil {
		return out, fmt.Errorf("parse page url: %w", err)
	}
	for _, asset := range pickAssets(base, pageBody, maxAssets) {
		body, assetType, err := assetFetcher(ctx, asset)
		if err != nil {
			continue // one unreachable asset costs a noted gap, not the scan
		}
		if !textEligible(assetType) {
			continue
		}
		body = body[:clampLen(len(body), maxAssetBytes)]
		for _, f := range Scan(body) {
			out = append(out, PageFinding{Asset: asset, Name: f.Name, Confidence: f.Confidence, Masked: f.Masked})
		}
		// A source map is fetched only when the script itself says it has one,
		// by carrying a sourceMappingURL marker in its own body.
		if strings.Contains(string(body), "sourceMappingURL") && strings.HasSuffix(asset, ".js") {
			mapBody, mapType, err := assetFetcher(ctx, asset+".map")
			if err != nil {
				continue
			}
			if !textEligible(mapType) {
				continue
			}
			mapBody = mapBody[:clampLen(len(mapBody), maxAssetBytes)]
			for _, f := range Scan(mapBody) {
				out = append(out, PageFinding{Asset: asset + ".map", Name: f.Name, Confidence: f.Confidence, Masked: f.Masked})
			}
		}
	}
	return out, nil
}

// guardedHTTPFetch is the production fetcher. The pre-flight CheckURL gives a
// refusal that can name the address; Transport holds the line at dial time,
// which is the check a DNS rebinding swap would try to get past.
func guardedHTTPFetch(ctx context.Context, target string) ([]byte, string, error) {
	if err := netguard.CheckURL(ctx, target); err != nil {
		return nil, "", err
	}
	client := &http.Client{
		Timeout:   perRequestBound,
		Transport: netguard.Transport(),
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "ceraa/0.1.0 (+go; scan-secrets)")
	req.Header.Set("Accept", "text/*, application/*")

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, resp.Header.Get("Content-Type"), fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes))
	if err != nil {
		return nil, resp.Header.Get("Content-Type"), fmt.Errorf("read body: %w", err)
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// pickAssets resolves the page's script and .js link references against the
// page URL, drops duplicates and non-HTTP targets, and returns same-origin
// assets ahead of any others, cut at cap.
func pickAssets(base *url.URL, pageBody []byte, cap int) []string {
	seen := map[string]bool{base.String(): true}
	var sameOrigin, elsewhere []string
	for _, re := range []*regexp.Regexp{scriptSrcRe, linkJSHrefRe} {
		for _, groups := range re.FindAllSubmatch(pageBody, -1) {
			ref, err := url.Parse(strings.TrimSpace(string(groups[1])))
			if err != nil {
				continue
			}
			resolved := base.ResolveReference(ref)
			resolved.Fragment = ""
			if resolved.Scheme != "http" && resolved.Scheme != "https" || resolved.Host == "" {
				continue
			}
			target := resolved.String()
			if seen[target] {
				continue
			}
			seen[target] = true
			if resolved.Host == base.Host {
				sameOrigin = append(sameOrigin, target)
			} else {
				elsewhere = append(elsewhere, target)
			}
		}
	}
	picked := sameOrigin
	if len(picked) < cap {
		room := cap - len(picked)
		if len(elsewhere) < room {
			room = len(elsewhere)
		}
		picked = append(picked, elsewhere[:room]...)
	}
	if len(picked) > cap {
		picked = picked[:cap]
	}
	return picked
}

// textEligible says whether a body should be scanned at all: text/* and
// application/* bodies go through the pattern table; image and other binary
// bodies are skipped. An absent content type fails closed.
func textEligible(contentType string) bool {
	mime := contentType
	if idx := strings.Index(mime, ";"); idx >= 0 {
		mime = mime[:idx]
	}
	mime = strings.ToLower(strings.TrimSpace(mime))
	return strings.HasPrefix(mime, "text/") || strings.HasPrefix(mime, "application/")
}

func clampLen(n, limit int) int {
	if n > limit {
		return limit
	}
	return n
}
