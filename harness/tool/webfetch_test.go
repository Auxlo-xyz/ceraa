package tool

import (
	"strings"
	"testing"
)

const webFetchSample = `<!doctype html>
<html><head><title>Test &amp; Title</title>
<style>body{color:red}</style>
<script>var secret = "AIza" + "notasecret";</script>
</head>
<body>
<nav><a href="/login">Log in</a><a href="/signup">Sign up</a></nav>
<main>
  <h1>Programme list</h1>
  <p>HackerOne is the <strong>largest</strong> programme catalog. It&#39;s first.</p>
  <ul>
    <li>Bugcrowd <em>second</em></li>
    <li><a href="/platforms/intigriti">Intigriti</a></li>
  </ul>
  <pre><code class="language-go">func main() {
	fmt.Println("keep  spaces")
}</code></pre>
  <table>
    <tr><th>Name</th><th>Scope</th></tr>
    <tr><td>VDP</td><td>web</td></tr>
  </table>
  <footer>Copyright footer junk</footer>
</main>
<aside>Cookie banner</aside>
</body></html>`

func TestWebFetchMarkdownKeepsStructure(t *testing.T) {
	got := renderHTML(webFetchSample, "https://example.com/list", false)
	for _, want := range []string{
		"# Programme list",
		"**largest**",
		"It's first",
		"- Bugcrowd *second*",
		"[Intigriti](https://example.com/platforms/intigriti)",
		"```go",
		"| Name | Scope |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown is missing %q\n---\n%s", want, got)
		}
	}
	if strings.Count(got, "\n") < 10 {
		t.Errorf("output has no line structure: %q", got)
	}
}

func TestWebFetchDropsScriptsNavAndAside(t *testing.T) {
	got := renderHTML(webFetchSample, "https://example.com/list", false)
	for _, unwanted := range []string{"color:red", "AIza", "Log in", "Sign up", "Cookie banner", "Copyright"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("markdown kept %q\n---\n%s", unwanted, got)
		}
	}
}

func TestWebFetchTextHasNoMarkdownSyntax(t *testing.T) {
	got := renderHTML(webFetchSample, "https://example.com/list", true)
	for _, unwanted := range []string{"# ", "**", "](", "```", "![", "| ---"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("text mode kept markdown syntax %q\n---\n%s", unwanted, got)
		}
	}
	if !strings.Contains(got, "Intigriti") || !strings.Contains(got, "It's first") {
		t.Errorf("text mode lost content: %q", got)
	}
}

func TestWebFetchHTMLIsVerbatim(t *testing.T) {
	got, err := convert(webFetchSample, "https://example.com/list", "text/html; charset=utf-8", "html")
	if err != nil {
		t.Fatal(err)
	}
	if got != webFetchSample {
		t.Error("format=html did not return the body unchanged")
	}
}

func TestWebFetchLeavesJSONAlone(t *testing.T) {
	body := `{"name":"example"}`
	got, err := convert(body, "https://example.com/api", "application/json", "markdown")
	if err != nil {
		t.Fatal(err)
	}
	if got != body {
		t.Errorf("JSON was rewritten: %q", got)
	}
}

func TestWebFetchPrefersMainOverABiggerBody(t *testing.T) {
	page := `<html><body>
	<p>` + strings.Repeat("nav junk ", 200) + `</p>
	<nav><p>` + strings.Repeat("menu ", 300) + `</p></nav>
	<main><h1>Real content</h1><p>Nothing else.</p></main>
	</body></html>`
	got := renderHTML(page, "https://example.com/", false)
	if !strings.Contains(got, "Real content") {
		t.Fatalf("main content missing: %q", got)
	}
	if strings.Contains(got, "nav junk") || strings.Contains(got, "menu") {
		t.Errorf("body chrome leaked in: %q", got)
	}
}

func TestWebFetchFallsBackToBodyWhenMainIsEmpty(t *testing.T) {
	page := `<html><body><main><span></span></main><p>` + strings.Repeat("real body text ", 40) + `</p></body></html>`
	got := renderHTML(page, "https://example.com/", false)
	if !strings.Contains(got, "real body text") {
		t.Errorf("an empty main hid the body: %q", got)
	}
}

func TestWebFetchCollapsesWhitespaceAndBlankLines(t *testing.T) {
	page := `<html><body><p>alpha    beta
	gamma</p><p>delta</p></body></html>`
	got := renderHTML(page, "https://example.com/", false)
	if !strings.Contains(got, "alpha beta gamma") {
		t.Errorf("inline whitespace was not collapsed: %q", got)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("blank lines stacked: %q", got)
	}
}

func TestWebFetchResolvesImagesAndSkipsJavascriptLinks(t *testing.T) {
	page := `<html><body><p><img src="/a.png" alt="Logo"><a href="javascript:alert(1)">click</a><a href="#top">top</a></p></body></html>`
	got := renderHTML(page, "https://example.com/dir/page", false)
	if !strings.Contains(got, "![Logo](https://example.com/a.png)") {
		t.Errorf("image not resolved: %q", got)
	}
	if strings.Contains(got, "javascript:") || strings.Contains(got, "](https://example.com/dir/page#top)") {
		t.Errorf("script or fragment link survived: %q", got)
	}
}

func TestWebFetchHandlesBrokenMarkup(t *testing.T) {
	got := renderHTML(`<p>unclosed <b>bold`, "https://example.com/", false)
	if !strings.Contains(got, "**bold**") {
		t.Errorf("broken markup lost the emphasis: %q", got)
	}
}

func TestWebFetchUsesTheTitleWhenThereIsNoHeading(t *testing.T) {
	page := `<html><head><title>Plain page</title></head><body><p>Body only.</p></body></html>`
	got := renderHTML(page, "https://example.com/", false)
	if !strings.HasPrefix(got, "# Plain page") {
		t.Errorf("title was not used as the heading: %q", got)
	}
}
