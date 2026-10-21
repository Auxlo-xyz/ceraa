package enum

// subdomains.go is the certificate transparency half of passive
// reconnaissance: the names a domain is known by, from the logs the world's
// certificate authorities publish into.
//
// The primary source is crt.sh, which is the most complete and the least
// reliable: it answers 502 often enough that a lookup trusting it alone fails
// half the time. The fallback is Cert Spotter, which answers the same
// question without a key and is far steadier. The result always names the
// source that answered, because a list with no provenance is a list nobody
// knows how much to trust.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

const (
	// sourceCRTSH is the primary certificate transparency source.
	sourceCRTSH = "crt.sh"
	// sourceCertSpotter is the fallback. It needs no key and answers the
	// same question, so a crt.sh outage costs a slower lookup, not a
	// failed one.
	sourceCertSpotter = "certspotter"
	// crtAttempts is how many times crt.sh is asked before the fallback
	// takes over. Two covers the 502s it serves under load, and few enough
	// that a dead source does not hold the caller for long.
	crtAttempts = 2
	// maxSubdomains bounds the name list. A domain with a thousand
	// certificates is real, and the list a model can use is not a thousand
	// lines, so the list is cut and the cut is reported.
	maxSubdomains = 500
)

// SubdomainResult is the answer plus its provenance.
type SubdomainResult struct {
	// Names are the host names the logs know, lower cased, deduplicated
	// and sorted.
	Names []string
	// Source names the log that answered.
	Source string
	// Truncated says the list was cut at the ceiling.
	Truncated bool
	// Notes carry the honest remarks about the answer: a cut list, an apex
	// that was dropped.
	Notes []string
}

// Subdomains returns the host names that certificate transparency logs know
// about for a domain.
//
// The domain is normalised first: a scheme, a path, a trailing dot and the
// case are all noise, and a model that writes "https://Example.com/admin" means
// one domain, not four.
func Subdomains(ctx context.Context, domain string) ([]string, error) {
	result, err := CollectSubdomains(ctx, Options{}, domain)
	if err != nil {
		return nil, err
	}
	return result.Names, nil
}

// CollectSubdomains is Subdomains with the source, the truncation flag and
// the injectable seams.
//
// The options are a parameter rather than a package global so a test can keep
// the run off the network without leaving a door open for the next test.
func CollectSubdomains(ctx context.Context, opts Options, domain string) (SubdomainResult, error) {
	clean, err := normaliseDomain(domain)
	if err != nil {
		return SubdomainResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, opts.timeout())
	defer cancel()

	entries, source, err := collectEntries(ctx, opts, clean)
	if err != nil {
		return SubdomainResult{}, err
	}

	names := namesFromEntries(entries, clean)
	result := SubdomainResult{Source: source}
	if len(names) > maxSubdomains {
		names = names[:maxSubdomains]
		result.Truncated = true
		result.Notes = append(result.Notes, fmt.Sprintf("the list was cut at %d names; the logs know more", maxSubdomains))
	}
	result.Names = names
	return result, nil
}

// collectEntries asks the primary source and falls back to the second one.
//
// Both failures are reported together: a caller that sees only the last
// error cannot tell a dead source from a dead network.
func collectEntries(ctx context.Context, opts Options, domain string) ([]ctEntry, string, error) {
	entries, err := fetchCT(ctx, opts, domain)
	if err == nil {
		return entries, sourceCRTSH, nil
	}
	fallback, fallbackErr := fetchCertSpotter(ctx, opts, domain)
	if fallbackErr != nil {
		return nil, "", fmt.Errorf("certificate transparency: %s: %v; %s: %v", sourceCRTSH, err, sourceCertSpotter, fallbackErr)
	}
	return fallback, sourceCertSpotter, nil
}

// fetchCT asks crt.sh, retrying once.
//
// The retry exists because crt.sh's outages are short and its answer is the
// better one. The backoff is short for the same reason: a caller waiting on
// a dead source is a caller not reconnoitring.
func fetchCT(ctx context.Context, opts Options, domain string) ([]ctEntry, error) {
	var last error
	for attempt := 0; attempt < crtAttempts; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, opts.backoff()); err != nil {
				return nil, fmt.Errorf("%s: %w", sourceCRTSH, err)
			}
		}
		_, body, err := fetch(ctx, opts, crtURL(domain), sourceCRTSH, 0)
		if err != nil {
			last = err
			continue
		}
		entries, err := decodeCT(body)
		if err != nil {
			last = fmt.Errorf("%s: %w", sourceCRTSH, err)
			continue
		}
		return entries, nil
	}
	if last == nil {
		// Unreachable while crtAttempts is above zero, and a silent empty
		// answer here would look like a domain with no certificates.
		last = fmt.Errorf("%s: no attempt was made", sourceCRTSH)
	}
	return nil, last
}

// fetchCertSpotter asks the fallback source once. It is the steadier of the
// two, so a failure here is worth reporting rather than retrying around.
func fetchCertSpotter(ctx context.Context, opts Options, domain string) ([]ctEntry, error) {
	_, body, err := fetch(ctx, opts, certSpotterURL(domain), sourceCertSpotter, 0)
	if err != nil {
		return nil, err
	}
	entries, err := decodeCT(body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", sourceCertSpotter, err)
	}
	return entries, nil
}

// ctEntry is one certificate in either shape. crt.sh writes the names in
// name_value and the subject in common_name; Cert Spotter writes them in
// dns_names. One struct reads both, because two parsers would drift.
type ctEntry struct {
	NameValue  string   `json:"name_value"`
	CommonName string   `json:"common_name"`
	DNSNames   []string `json:"dns_names"`
}

// decodeCT reads the array both sources answer with.
func decodeCT(body []byte) ([]ctEntry, error) {
	var entries []ctEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("the answer was not the expected JSON array: %w", err)
	}
	return entries, nil
}

// namesFromEntries extracts the clean name set.
//
// A name is kept only when it is the domain or a subdomain of it: a
// certificate carries subject alternative names for other domains, and those
// are not what was asked for.
func namesFromEntries(entries []ctEntry, domain string) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(raw string, fromSAN bool) {
		name, wildcard := cleanName(raw)
		if name == "" {
			return
		}
		if name != domain && !strings.HasSuffix(name, "."+domain) {
			return
		}
		if name == domain && (wildcard || !fromSAN) {
			// The apex is not a subdomain. It survives only when the logs
			// carry it as a subject alternative name, where the
			// certificate was issued for the name itself rather than for
			// a wildcard over it or merely for the subject.
			return
		}
		if seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, entry := range entries {
		for _, part := range strings.Split(entry.NameValue, "\n") {
			add(part, true)
		}
		for _, name := range entry.DNSNames {
			add(name, true)
		}
		add(entry.CommonName, false)
	}
	sort.Strings(out)
	return out
}

// cleanName normalises one certificate name and reports whether it was a
// wildcard. A wildcard says "and everything under this", which is a hint
// about a name rather than proof of it.
func cleanName(raw string) (string, bool) {
	name := strings.ToLower(strings.TrimSpace(raw))
	name = strings.TrimRight(name, ".")
	if name == "" {
		return "", false
	}
	wildcard := strings.HasPrefix(name, "*.")
	name = strings.TrimPrefix(name, "*.")
	return strings.TrimSpace(name), wildcard
}

// normaliseDomain reduces what a model typed to the domain it meant.
//
// The scheme, the userinfo, the path, the port and the trailing dot are all
// ways of writing the same domain, and a certificate query built on the wrong
// one answers about a domain nobody asked for.
func normaliseDomain(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("domain is required")
	}
	if strings.ContainsAny(trimmed, " \t\r\n") {
		return "", fmt.Errorf("domain %q contains whitespace", raw)
	}
	// Scheme.
	if index := strings.Index(trimmed, "://"); index >= 0 {
		trimmed = trimmed[index+3:]
	}
	// Userinfo.
	if index := strings.LastIndex(trimmed, "@"); index >= 0 {
		trimmed = trimmed[index+1:]
	}
	// Path, query and fragment.
	if index := strings.IndexAny(trimmed, "/?#"); index >= 0 {
		trimmed = trimmed[:index]
	}
	// Port.
	if index := strings.LastIndex(trimmed, ":"); index >= 0 && isDigits(trimmed[index+1:]) {
		trimmed = trimmed[:index]
	}
	domain := strings.ToLower(strings.TrimSpace(trimmed))
	domain = strings.TrimRight(domain, ".")
	if domain == "" {
		return "", fmt.Errorf("domain %q is empty once the scheme, the path and the trailing dot are removed", raw)
	}
	if strings.ContainsAny(domain, " \t\r\n") {
		return "", fmt.Errorf("domain %q contains whitespace", raw)
	}
	return domain, nil
}

// isDigits reports whether a string is only digits, so a port can be told
// from a name that happens to carry a colon.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// crtURL is the primary source's query. The wildcard asks for the domain and
// everything under it.
func crtURL(domain string) string {
	return "https://crt.sh/?q=" + url.QueryEscape("%."+domain) + "&output=json"
}

// certSpotterURL is the fallback source's query.
func certSpotterURL(domain string) string {
	return "https://api.certspotter.com/v1/issuances?domain=" + url.QueryEscape(domain) +
		"&include_subdomains=true&expand=dns_names"
}
