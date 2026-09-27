package telegram

import (
	"fmt"
	"sort"
	"strings"

	"ceraa/harness/sessionstore"
)

// Per-endpoint cache accounting.
//
// The question "is this provider actually caching?" cannot be answered from an
// aggregate, because a single session mixes routes and a single bad route can drag
// the average to a number that means nothing. It also cannot be answered at all
// from runs whose counts were rejected, which is why the rejected ones are
// counted separately instead of being quietly dropped: "we do not know" and "it
// cached nothing" are different answers and collapsing them is how a broken route
// looks like a working one.

// cacheEndpointStat is one endpoint's aggregate cache behaviour.
type cacheEndpointStat struct {
	Endpoint string
	Model    string
	Runs     int
	Trusted  int
	Rejected int
	Input    int
	Cached   int
}

// HitRate is the token-weighted cache read ratio, which is the only fair way to
// average it: a run of 20K tokens and a run of 2M do not count equally.
func (s cacheEndpointStat) HitRate() float64 {
	if s.Input <= 0 {
		return 0
	}
	return float64(s.Cached) / float64(s.Input)
}

// aggregateCache groups runs by endpoint and model.
//
// Only trusted runs contribute to the token totals. A rejected run still appears,
// with its own count, so the report can say that an endpoint exists and that its
// behaviour is unknown rather than implying it cached nothing.
func aggregateCache(runs []sessionstore.Run) []cacheEndpointStat {
	byKey := map[string]*cacheEndpointStat{}
	for i := range runs {
		run := &runs[i]
		host := run.ProviderHost
		if strings.TrimSpace(host) == "" {
			host = "(unrecorded)"
		}
		model := run.PricingModel
		if strings.TrimSpace(model) == "" {
			model = "(unrecorded)"
		}
		key := host + "\x00" + model
		stat, ok := byKey[key]
		if !ok {
			stat = &cacheEndpointStat{Endpoint: host, Model: model}
			byKey[key] = stat
		}
		stat.Runs++
		if !run.UsageTrust.Trusted {
			stat.Rejected++
			continue
		}
		stat.Trusted++
		stat.Input += run.Usage.InputTokens
		stat.Cached += run.Usage.CachedInputTokens
	}

	stats := make([]cacheEndpointStat, 0, len(byKey))
	for _, stat := range byKey {
		stats = append(stats, *stat)
	}
	// Most-cached first, because the point is to find a route that works. Routes
	// with nothing measured sort last rather than masquerading as 0%.
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].Trusted != stats[j].Trusted {
			return stats[i].Trusted > stats[j].Trusted
		}
		if stats[i].HitRate() != stats[j].HitRate() {
			return stats[i].HitRate() > stats[j].HitRate()
		}
		return stats[i].Endpoint < stats[j].Endpoint
	})
	return stats
}

// cacheReport renders per-endpoint cache behaviour for a session.
func cacheReport(sess *sessionstore.Session) string {
	if sess == nil || len(sess.Runs) == 0 {
		return "📇 Cache\nNo runs recorded yet."
	}
	stats := aggregateCache(sess.Runs)

	var b strings.Builder
	b.WriteString("📇 Cache by endpoint\n")
	b.WriteString("Token-weighted cache read, trusted runs only.\n")
	totalTrusted, totalRejected := 0, 0
	for _, stat := range stats {
		totalTrusted += stat.Trusted
		totalRejected += stat.Rejected
	}
	fmt.Fprintf(&b, "\n%d of %d runs measured, %d rejected\n", totalTrusted, totalTrusted+totalRejected, totalRejected)

	for _, stat := range stats {
		if stat.Trusted == 0 {
			fmt.Fprintf(&b, "\n%s · %s\n  unknown: %d run(s), provider counts unusable\n",
				shortHost(stat.Endpoint), stat.Model, stat.Rejected)
			continue
		}
		fmt.Fprintf(&b, "\n%s · %s\n  %.1f%% cache read over %d run(s), %s of %s input\n",
			shortHost(stat.Endpoint), stat.Model, stat.HitRate()*100, stat.Trusted,
			groupDigits(stat.Cached), groupDigits(stat.Input))
		if stat.Rejected > 0 {
			fmt.Fprintf(&b, "  %d further run(s) rejected as unusable\n", stat.Rejected)
		}
	}
	return b.String()
}

// shortHost trims a base URL down to something that fits a chat bubble, and never
// shows a query string or credentials if one was ever embedded in the URL.
func shortHost(raw string) string {
	host := raw
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	if i := strings.IndexAny(host, "/?"); i >= 0 {
		host = host[:i]
	}
	if i := strings.Index(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	if host == "" {
		return "(unrecorded)"
	}
	return host
}
