package findings

// report.go is the disclosure side of the record: the CVSS arithmetic a
// severity claim rests on, and the two renderings of the record a triager
// reads.
//
// The renderings are deliberately plain. A disclosure document is read by
// somebody deciding whether to send it, and a document that needs a viewer to
// be legible is a document that will be skimmed, not read.

import (
	"fmt"
	"html"
	"math"
	"strings"
)

// Metrics is the eight CVSS v3.1 base metrics.
//
// AV is the attack vector, AC the attack complexity, PR the privileges
// required, UI the user interaction, S the scope, and C, I and A the
// confidentiality, integrity and availability impacts.
type Metrics struct {
	AV, AC, PR, UI, S, C, I, A string
}

// Vector renders the metrics as a CVSS v3.1 vector string.
//
// The values are uppercased on the way out, so the vector is canonical
// whatever case the metrics arrived in and two equal metrics always render
// to the same string.
func (m Metrics) Vector() string {
	return fmt.Sprintf("CVSS:3.1/AV:%s/AC:%s/PR:%s/UI:%s/S:%s/C:%s/I:%s/A:%s",
		strings.ToUpper(strings.TrimSpace(m.AV)),
		strings.ToUpper(strings.TrimSpace(m.AC)),
		strings.ToUpper(strings.TrimSpace(m.PR)),
		strings.ToUpper(strings.TrimSpace(m.UI)),
		strings.ToUpper(strings.TrimSpace(m.S)),
		strings.ToUpper(strings.TrimSpace(m.C)),
		strings.ToUpper(strings.TrimSpace(m.I)),
		strings.ToUpper(strings.TrimSpace(m.A)))
}

// The v3.1 base metric weights.
var (
	avWeights = map[string]float64{"N": 0.85, "A": 0.62, "L": 0.55, "P": 0.2}
	acWeights = map[string]float64{"L": 0.77, "H": 0.44}
	// PR is the one metric whose weight depends on the scope. The same
	// privilege is worth more when the attack crosses a security boundary,
	// which is why the table is chosen by the scope and not by the metric.
	prWeightsUnchanged = map[string]float64{"N": 0.85, "L": 0.62, "H": 0.27}
	prWeightsChanged   = map[string]float64{"N": 0.85, "L": 0.68, "H": 0.50}
	uiWeights          = map[string]float64{"N": 0.85, "R": 0.62}
	ciaWeights         = map[string]float64{"H": 0.56, "L": 0.22, "N": 0}
)

// The accepted values, in the order the specification lists them, so an
// error message can name them.
var (
	avValues  = []string{"N", "A", "L", "P"}
	acValues  = []string{"L", "H"}
	prValues  = []string{"N", "L", "H"}
	uiValues  = []string{"N", "R"}
	ciaValues = []string{"H", "L", "N"}
)

// Score computes the CVSS v3.1 base score.
//
// The published formula, in order: the impact sub-score from the three CIA
// metrics, the exploitability sub-score from the four temporal-ish metrics,
// the 1.08 multiplier when the scope changed, and the Roundup to one decimal.
// A zero impact is zero score, because a finding that affects nothing cannot
// be exploited.
func (m Metrics) Score() (float64, error) {
	scope := strings.ToUpper(strings.TrimSpace(m.S))
	if scope != "U" && scope != "C" {
		return 0, fmt.Errorf("cvss scope %q is not one of U, C", m.S)
	}
	av, err := metricValue(avWeights, avValues, "AV", m.AV)
	if err != nil {
		return 0, err
	}
	ac, err := metricValue(acWeights, acValues, "AC", m.AC)
	if err != nil {
		return 0, err
	}
	prTable := prWeightsUnchanged
	if scope == "C" {
		prTable = prWeightsChanged
	}
	pr, err := metricValue(prTable, prValues, "PR", m.PR)
	if err != nil {
		return 0, err
	}
	ui, err := metricValue(uiWeights, uiValues, "UI", m.UI)
	if err != nil {
		return 0, err
	}
	confidentiality, err := metricValue(ciaWeights, ciaValues, "C", m.C)
	if err != nil {
		return 0, err
	}
	integrity, err := metricValue(ciaWeights, ciaValues, "I", m.I)
	if err != nil {
		return 0, err
	}
	availability, err := metricValue(ciaWeights, ciaValues, "A", m.A)
	if err != nil {
		return 0, err
	}

	// The impact sub-score: the independent combination of the three impacts,
	// scaled by the scope.
	iss := 1 - ((1 - confidentiality) * (1 - integrity) * (1 - availability))
	impact := 0.0
	if scope == "U" {
		impact = 6.42 * iss
	} else {
		impact = 7.52*(iss-0.029) - 3.25*math.Pow(iss-0.02, 15)
	}
	if impact <= 0 {
		// Nothing is affected, so there is nothing to exploit.
		return 0, nil
	}

	// The exploitability sub-score: what the attacker has to bring to it.
	exploitability := 8.22 * av * ac * pr * ui

	base := impact + exploitability
	if scope == "C" {
		// A scope change is worth more than the same primitives inside one
		// authority, so the whole score is scaled.
		base = 1.08 * base
	}
	if base > 10 {
		base = 10
	}
	return roundup(base), nil
}

// metricValue reads one metric against its weight table.
func metricValue(weights map[string]float64, accepted []string, name, raw string) (float64, error) {
	value := strings.ToUpper(strings.TrimSpace(raw))
	if value == "" {
		return 0, fmt.Errorf("cvss metric %s is missing", name)
	}
	weight, ok := weights[value]
	if !ok {
		return 0, fmt.Errorf("cvss metric %s=%q is not one of %s", name, raw, strings.Join(accepted, ", "))
	}
	return weight, nil
}

// roundup is the CVSS Roundup: the value is truncated to one decimal and then
// incremented to the next tenth, except when it already sits on a tenth.
//
// The integer guard is the specification's own, and it is what keeps 9.8 at
// 9.8 instead of pushing it to 9.9 through a floating point remainder.
func roundup(value float64) float64 {
	scaled := int(math.Round(value * 100000))
	if scaled%10000 == 0 {
		return float64(scaled) / 100000
	}
	return (math.Floor(float64(scaled)/10000) + 1) / 10
}

// ParseVector reads a CVSS v3.1 base vector into its metrics.
//
// The "CVSS:3.1" prefix is optional, because the eight metrics are what the
// score is made of and a caller who has them does not need the version to
// say so. A version that is present but different is refused: the base
// formula changed between versions and a score computed under the wrong one
// is a number that looks right and is not.
func ParseVector(vector string) (Metrics, error) {
	trimmed := strings.TrimSpace(vector)
	if trimmed == "" {
		return Metrics{}, fmt.Errorf("the cvss vector is empty")
	}
	metrics := Metrics{}
	seen := map[string]bool{}
	for _, part := range strings.Split(trimmed, "/") {
		key, value, ok := strings.Cut(part, ":")
		if !ok {
			return Metrics{}, fmt.Errorf("cvss vector part %q is not KEY:VALUE", part)
		}
		key = strings.ToUpper(strings.TrimSpace(key))
		value = strings.ToUpper(strings.TrimSpace(value))
		switch key {
		case "CVSS":
			if value != "3.1" && value != "3.0" {
				return Metrics{}, fmt.Errorf("cvss version %q is not 3.1", value)
			}
			continue
		case "AV":
			metrics.AV = value
		case "AC":
			metrics.AC = value
		case "PR":
			metrics.PR = value
		case "UI":
			metrics.UI = value
		case "S":
			metrics.S = value
		case "C":
			metrics.C = value
		case "I":
			metrics.I = value
		case "A":
			metrics.A = value
		default:
			return Metrics{}, fmt.Errorf("cvss metric %q is not one of AV, AC, PR, UI, S, C, I, A", key)
		}
		if seen[key] {
			return Metrics{}, fmt.Errorf("cvss metric %q is given twice", key)
		}
		seen[key] = true
	}
	return metrics, nil
}

// CVSSScore computes the CVSS v3.1 base score of a vector string.
func CVSSScore(vector string) (float64, error) {
	metrics, err := ParseVector(vector)
	if err != nil {
		return 0, err
	}
	return metrics.Score()
}

// ---------------------------------------------------------------------------
// The markdown disclosure.
// ---------------------------------------------------------------------------

// RenderMarkdown renders the record as a disclosure document.
//
// The order is the argument: the summary, so a reader knows what is there;
// the findings; then the two sections that say what was NOT established; then
// the counts. A reader who stops after the findings has still been shown that
// gaps and ruled-out items exist, which is the difference between a report
// and a claim.
func RenderMarkdown(list []Finding, coverage Coverage) string {
	var b strings.Builder
	b.WriteString("# Security finding report\n\n")
	b.WriteString("A finding is a result only when it is confirmed. The open proof gaps and the ruled out items below are part of the record and are reported with it, because a report that hides them cannot tell the reader what was checked.\n\n")

	b.WriteString("## Summary\n\n")
	if len(list) == 0 {
		b.WriteString("No findings recorded.\n\n")
	} else {
		b.WriteString("| Title | Severity | Status | URL |\n")
		b.WriteString("| --- | --- | --- | --- |\n")
		for _, f := range list {
			b.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n",
				mdCell(f.Title), mdCell(f.Severity), mdCell(string(f.Status)), mdCell(f.URL)))
		}
		b.WriteString("\n")
	}

	b.WriteString("## Findings\n\n")
	if len(list) == 0 {
		b.WriteString("Nothing was recorded.\n\n")
	}
	for _, f := range list {
		writeMarkdownFinding(&b, f)
	}

	writeMarkdownList(&b, "Open proof gaps", coverage.OpenGaps, list)
	writeMarkdownList(&b, "Ruled out", coverage.RuledOut, list)

	b.WriteString("## Coverage\n\n")
	b.WriteString(fmt.Sprintf("- findings: %d\n", coverage.Total))
	for _, name := range statusNames {
		b.WriteString(fmt.Sprintf("- %s: %d\n", name, coverage.ByStatus[Status(name)]))
	}
	for _, name := range severityNames {
		if count := coverage.BySeverity[name]; count > 0 {
			b.WriteString(fmt.Sprintf("- %s: %d\n", name, count))
		}
	}
	return b.String()
}

// writeMarkdownFinding writes one finding's section. A field that is empty is
// skipped, so a finding with nothing but a title still renders as a title.
func writeMarkdownFinding(b *strings.Builder, f Finding) {
	b.WriteString(fmt.Sprintf("### %s\n\n", mdLine(f.Title)))
	b.WriteString(fmt.Sprintf("- id: `%s`\n", f.ID))
	b.WriteString(fmt.Sprintf("- severity: %s\n", f.Severity))
	b.WriteString(fmt.Sprintf("- status: %s\n", f.Status))
	if f.URL != "" {
		b.WriteString(fmt.Sprintf("- url: %s\n", f.URL))
	}
	if f.Endpoint != "" {
		b.WriteString(fmt.Sprintf("- endpoint: %s\n", mdLine(f.Endpoint)))
	}
	if f.CVSSVector != "" || f.CVSSScore > 0 {
		b.WriteString(fmt.Sprintf("- cvss: %s\n", cvssLine(f)))
	}
	if len(f.Tags) > 0 {
		b.WriteString(fmt.Sprintf("- tags: %s\n", strings.Join(f.Tags, ", ")))
	}
	b.WriteString("\n")
	if f.Description != "" {
		b.WriteString(f.Description + "\n\n")
	}
	if f.PoC != "" {
		b.WriteString("Proof of concept:\n\n")
		b.WriteString(Fence(f.PoC) + "\n\n")
	}
	if f.Evidence != "" {
		b.WriteString("Evidence:\n\n")
		b.WriteString(f.Evidence + "\n\n")
	}
	if f.Counterevidence != "" {
		b.WriteString("Counterevidence:\n\n")
		b.WriteString(f.Counterevidence + "\n\n")
	}
	if f.Remediation != "" {
		b.WriteString("Remediation:\n\n")
		b.WriteString(f.Remediation + "\n\n")
	}
	if !f.UpdatedAt.IsZero() {
		b.WriteString(fmt.Sprintf("- last updated: %s\n\n", f.UpdatedAt.UTC().Format("2006-01-02 15:04:05 UTC")))
	}
}

// writeMarkdownList writes the gap and ruled-out sections. The titles come
// from the coverage, and the detail from the record, because the coverage
// says what exists and the finding says why.
func writeMarkdownList(b *strings.Builder, heading string, titles []string, list []Finding) {
	b.WriteString(fmt.Sprintf("## %s\n\n", heading))
	if len(titles) == 0 {
		b.WriteString("None recorded.\n\n")
		return
	}
	for _, title := range titles {
		f, ok := findByTitle(list, title)
		if !ok {
			b.WriteString(fmt.Sprintf("- %s\n", mdLine(title)))
			continue
		}
		b.WriteString(fmt.Sprintf("- **%s** (%s, `%s`)\n", mdLine(f.Title), f.Severity, f.ID))
		if f.Description != "" {
			for _, line := range strings.Split(strings.TrimRight(f.Description, "\n"), "\n") {
				b.WriteString("  " + line + "\n")
			}
		}
	}
	b.WriteString("\n")
}

// findByTitle looks a finding up by title in the record.
func findByTitle(list []Finding, title string) (Finding, bool) {
	for _, f := range list {
		if f.Title == title {
			return f, true
		}
	}
	return Finding{}, false
}

// cvssLine renders the score and the vector it came from.
func cvssLine(f Finding) string {
	if f.CVSSVector == "" {
		return fmt.Sprintf("%.1f", f.CVSSScore)
	}
	return fmt.Sprintf("%.1f (%s)", f.CVSSScore, f.CVSSVector)
}

// mdLine collapses a value onto one line, so a title with a newline in it
// cannot break the heading it sits in.
func mdLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// mdCell collapses a value for a table cell and escapes the column separator,
// because a pipe in a title would otherwise end the cell early and shift
// every column after it.
func mdCell(value string) string {
	return strings.ReplaceAll(mdLine(value), "|", "\\|")
}

// Fence wraps text in a code fence long enough to contain any fence inside
// it, so a PoC carrying its own backticks does not end the block early.
//
// The reproduction is carried verbatim everywhere it appears, and a verbatim
// copy of a script that contains a fence needs a longer fence around it.
func Fence(text string) string {
	longest, current := 0, 0
	for _, r := range text {
		if r == '`' {
			current++
			if current > longest {
				longest = current
			}
			continue
		}
		current = 0
	}
	ticks := strings.Repeat("`", max(3, longest+1))
	return ticks + "\n" + strings.TrimRight(text, "\n") + "\n" + ticks
}

// ---------------------------------------------------------------------------
// The HTML disclosure.
// ---------------------------------------------------------------------------

// RenderHTML renders the record as a standalone HTML document.
//
// Everything a finding carries is escaped, nothing is fetched, and there is no
// script: the report is a file somebody opens, and a file that can run code
// is a file nobody opens twice.
func RenderHTML(list []Finding, coverage Coverage) string {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n")
	b.WriteString("<meta charset=\"utf-8\">\n")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	b.WriteString("<title>Security finding report</title>\n")
	b.WriteString(reportStyle)
	b.WriteString("</head>\n<body>\n<main>\n")

	b.WriteString("<h1>Security finding report</h1>\n")
	b.WriteString("<p class=\"note\">A finding is a result only when it is confirmed. The open proof gaps and the ruled out items below are part of the record and are reported with it, because a report that hides them cannot tell the reader what was checked.</p>\n")

	b.WriteString("<h2>Summary</h2>\n")
	if len(list) == 0 {
		b.WriteString("<p class=\"empty\">No findings recorded.</p>\n")
	} else {
		b.WriteString("<table>\n<thead><tr><th>Title</th><th>Severity</th><th>Status</th><th>URL</th></tr></thead>\n<tbody>\n")
		for _, f := range list {
			b.WriteString("<tr>")
			b.WriteString("<td>" + esc(f.Title) + "</td>")
			b.WriteString("<td>" + esc(f.Severity) + "</td>")
			b.WriteString("<td>" + esc(string(f.Status)) + "</td>")
			b.WriteString("<td>" + htmlURL(f.URL) + "</td>")
			b.WriteString("</tr>\n")
		}
		b.WriteString("</tbody>\n</table>\n")
	}

	b.WriteString("<h2>Findings</h2>\n")
	if len(list) == 0 {
		b.WriteString("<p class=\"empty\">Nothing was recorded.</p>\n")
	}
	for _, f := range list {
		writeHTMLFinding(&b, f)
	}

	writeHTMLList(&b, "Open proof gaps", coverage.OpenGaps, list)
	writeHTMLList(&b, "Ruled out", coverage.RuledOut, list)

	b.WriteString("<h2>Coverage</h2>\n<ul class=\"coverage\">\n")
	b.WriteString(fmt.Sprintf("<li>findings: %d</li>\n", coverage.Total))
	for _, name := range statusNames {
		b.WriteString(fmt.Sprintf("<li>%s: %d</li>\n", esc(name), coverage.ByStatus[Status(name)]))
	}
	for _, name := range severityNames {
		if count := coverage.BySeverity[name]; count > 0 {
			b.WriteString(fmt.Sprintf("<li>%s: %d</li>\n", esc(name), count))
		}
	}
	b.WriteString("</ul>\n")

	b.WriteString("</main>\n</body>\n</html>\n")
	return b.String()
}

// writeHTMLFinding writes one finding's section.
func writeHTMLFinding(b *strings.Builder, f Finding) {
	b.WriteString("<section class=\"finding\">\n")
	b.WriteString("<h3>" + esc(f.Title) + "</h3>\n")
	b.WriteString("<dl>\n")
	b.WriteString("<dt>id</dt><dd><code>" + esc(f.ID) + "</code></dd>\n")
	b.WriteString("<dt>severity</dt><dd>" + esc(f.Severity) + "</dd>\n")
	b.WriteString("<dt>status</dt><dd>" + esc(string(f.Status)) + "</dd>\n")
	if f.URL != "" {
		b.WriteString("<dt>url</dt><dd>" + htmlURL(f.URL) + "</dd>\n")
	}
	if f.Endpoint != "" {
		b.WriteString("<dt>endpoint</dt><dd>" + esc(f.Endpoint) + "</dd>\n")
	}
	if f.CVSSVector != "" || f.CVSSScore > 0 {
		b.WriteString("<dt>cvss</dt><dd>" + esc(cvssLine(f)) + "</dd>\n")
	}
	if len(f.Tags) > 0 {
		b.WriteString("<dt>tags</dt><dd>" + esc(strings.Join(f.Tags, ", ")) + "</dd>\n")
	}
	if !f.UpdatedAt.IsZero() {
		b.WriteString("<dt>last updated</dt><dd>" + esc(f.UpdatedAt.UTC().Format("2006-01-02 15:04:05 UTC")) + "</dd>\n")
	}
	b.WriteString("</dl>\n")
	if f.Description != "" {
		b.WriteString("<p>" + esc(f.Description) + "</p>\n")
	}
	if f.PoC != "" {
		b.WriteString("<h4>Proof of concept</h4>\n<pre><code>" + esc(f.PoC) + "</code></pre>\n")
	}
	if f.Evidence != "" {
		b.WriteString("<h4>Evidence</h4>\n<p>" + esc(f.Evidence) + "</p>\n")
	}
	if f.Counterevidence != "" {
		b.WriteString("<h4>Counterevidence</h4>\n<p>" + esc(f.Counterevidence) + "</p>\n")
	}
	if f.Remediation != "" {
		b.WriteString("<h4>Remediation</h4>\n<p>" + esc(f.Remediation) + "</p>\n")
	}
	b.WriteString("</section>\n")
}

// writeHTMLList writes the gap and ruled-out sections.
func writeHTMLList(b *strings.Builder, heading string, titles []string, list []Finding) {
	b.WriteString("<h2>" + esc(heading) + "</h2>\n")
	if len(titles) == 0 {
		b.WriteString("<p class=\"empty\">None recorded.</p>\n")
		return
	}
	b.WriteString("<ul>\n")
	for _, title := range titles {
		f, ok := findByTitle(list, title)
		if !ok {
			b.WriteString("<li>" + esc(title) + "</li>\n")
			continue
		}
		b.WriteString("<li><strong>" + esc(f.Title) + "</strong> <span class=\"meta\">(" + esc(f.Severity) + ", <code>" + esc(f.ID) + "</code>)</span>")
		if f.Description != "" {
			b.WriteString("<br>" + esc(f.Description))
		}
		b.WriteString("</li>\n")
	}
	b.WriteString("</ul>\n")
}

// htmlURL renders a URL. A link is only made for an http or https URL: a
// finding's URL is model-supplied text, and a javascript: URL in an href is
// a script in the report.
func htmlURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	escaped := esc(trimmed)
	if strings.HasPrefix(strings.ToLower(trimmed), "http://") || strings.HasPrefix(strings.ToLower(trimmed), "https://") {
		return "<a href=\"" + escaped + "\">" + escaped + "</a>"
	}
	return escaped
}

// esc escapes a value for HTML. Everything a finding carries goes through
// it: the record is model-written text and a report that renders it raw is a
// report that can carry a tag.
func esc(value string) string {
	return html.EscapeString(value)
}

// reportStyle is the whole stylesheet: inline, so the document is one file
// with nothing to fetch and nothing that can phone home.
const reportStyle = `<style>
:root { color-scheme: light dark; }
body { margin: 0; padding: 2rem 1.25rem 4rem; background: #ffffff; color: #121110;
  font: 16px/1.55 -apple-system, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; }
main { max-width: 62rem; margin: 0 auto; }
h1 { font-size: 1.6rem; margin: 0 0 .6rem; }
h2 { font-size: 1.15rem; margin: 2.4rem 0 .6rem; padding-bottom: .3rem; border-bottom: 1px solid #e1e1e1; }
h3 { font-size: 1.02rem; margin: 0 0 .5rem; }
h4 { font-size: .78rem; text-transform: uppercase; letter-spacing: .08em; color: #595959; margin: 1rem 0 .3rem; }
p { margin: .5rem 0; }
.note { color: #595959; max-width: 46rem; }
.empty { color: #595959; font-style: italic; }
.meta { color: #595959; font-size: .9em; }
table { border-collapse: collapse; width: 100%; font-size: .93rem; }
th, td { text-align: left; padding: .4rem .6rem; border-bottom: 1px solid #e1e1e1; vertical-align: top; }
th { font-weight: 600; }
dl { display: grid; grid-template-columns: max-content 1fr; gap: .15rem .9rem; margin: .5rem 0; font-size: .93rem; }
dt { color: #595959; }
dd { margin: 0; overflow-wrap: anywhere; }
pre { background: #f1f1f1; padding: .8rem; border-radius: 6px; overflow-x: auto; }
code { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: .88rem; }
ul { padding-left: 1.2rem; }
li { margin: .35rem 0; }
.finding { margin: 1.6rem 0 2rem; padding-bottom: 1rem; border-bottom: 1px solid #e1e1e1; }
.coverage { list-style: none; padding-left: 0; }
a { color: inherit; }
</style>
`
