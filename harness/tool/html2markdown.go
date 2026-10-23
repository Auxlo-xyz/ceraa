package tool

// html2markdown.go turns a fetched HTML document into text a model can read.
//
// The old converter stripped every tag and then joined all whitespace, which
// turned a page into one enormous line: headings, list items and paragraphs
// ran together, entity codes survived, and script bodies were kept as if they
// were prose. A 436 KB Immunefi page became 238 KB of concatenated text with
// the word "markdown" nowhere in it, while the tool schema claimed markdown was
// the default. This file makes the claim true.
//
// Two rules matter. Block structure becomes newlines and list markers instead
// of vanishing, and a text node's whitespace is collapsed there and only there,
// so preformatted code keeps its own indentation.

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// markdownCharLimit bounds what one fetch can put in a transcript. The original
// body is capped at 5 MB, and a page that large converts to more text than any
// turn can use; the tail says so rather than arriving as a silent cut.
const markdownCharLimit = 256 * 1024

// renderHTML converts an HTML body to markdown (plain == false) or plain text.
// base resolves relative links so a citation is usable outside the page.
func renderHTML(body, pageURL string, plain bool) string {
	doc, err := html.Parse(bytes.NewReader([]byte(body)))
	if err != nil {
		return collapseBlankLines(strings.TrimSpace(body))
	}
	var base *url.URL
	if pageURL != "" {
		if parsed, parseErr := url.Parse(pageURL); parseErr == nil {
			base = parsed
		}
	}
	renderer := &htmlRenderer{base: base, plain: plain}
	renderer.block(contentRoot(doc))
	out := collapseBlankLines(strings.TrimSpace(renderer.sb.String()))
	if title := documentTitle(doc); title != "" && !strings.HasPrefix(out, "# ") {
		if plain {
			out = title + "\n\n" + out
		} else {
			out = "# " + title + "\n\n" + out
		}
	}
	if len(out) > markdownCharLimit {
		out = out[:markdownCharLimit] + fmt.Sprintf("\n\n[truncated at %d characters]", markdownCharLimit)
	}
	return out
}

// contentRoot prefers the element that holds the article. Pages that are mostly
// chrome are why this exists: without it a page's menu and cookie banner arrive
// ahead of the content and cost more tokens than the answer.
func contentRoot(doc *html.Node) *html.Node {
	body := findFirst(doc, atom.Body)
	if body == nil {
		return doc
	}
	// An empty main is an SPA shell or a layout placeholder, and the content is
	// somewhere the markup does not admit. A short but non-empty main is the
	// author's answer, so it wins; second-guessing it hides real content.
	const minMainText = 20
	for _, candidate := range []atom.Atom{atom.Main, atom.Article} {
		best := largestNode(findAll(body, candidate))
		if best == nil {
			continue
		}
		if len(strings.TrimSpace(nodeText(best))) < minMainText {
			continue
		}
		return best
	}
	return body
}

func largestNode(nodes []*html.Node) *html.Node {
	var best *html.Node
	bestSize := 0
	for _, node := range nodes {
		if size := len(nodeText(node)); size > bestSize {
			best, bestSize = node, size
		}
	}
	return best
}

func findFirst(root *html.Node, target atom.Atom) *html.Node {
	nodes := findAll(root, target)
	if len(nodes) == 0 {
		return nil
	}
	return nodes[0]
}

func findAll(root *html.Node, target atom.Atom) []*html.Node {
	var found []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.DataAtom == target {
			found = append(found, n)
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return found
}

func nodeText(n *html.Node) string {
	var builder strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			builder.WriteString(node.Data)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	return builder.String()
}

func documentTitle(doc *html.Node) string {
	title := findFirst(doc, atom.Title)
	if title == nil {
		return ""
	}
	return strings.TrimSpace(collapseSpaces(nodeText(title)))
}

// skippedElements are content-free or hostile to a transcript: scripts, styles,
// embedded players, form controls and the page's navigation furniture.
var skippedElements = map[atom.Atom]bool{
	atom.Script:   true,
	atom.Style:    true,
	atom.Noscript: true,
	atom.Template: true,
	atom.Svg:      true,
	atom.Canvas:   true,
	atom.Iframe:   true,
	atom.Video:    true,
	atom.Audio:    true,
	atom.Object:   true,
	atom.Embed:    true,
	atom.Map:      true,
	atom.Nav:      true,
	atom.Aside:    true,
	atom.Footer:   true,
	atom.Form:     true,
	atom.Button:   true,
	atom.Select:   true,
	atom.Input:    true,
	atom.Textarea: true,
}

type htmlRenderer struct {
	sb    strings.Builder
	base  *url.URL
	plain bool
	pre   int
	depth int
	// trailingNewlines is the count of newlines already at the end of sb. It is
	// tracked rather than read back because calling sb.String() per block would
	// copy the whole document and turn a 200 KB page into quadratic work.
	trailingNewlines int
}

func (r *htmlRenderer) write(text string) {
	r.sb.WriteString(text)
	if text == "" {
		return
	}
	count := 0
	for i := len(text) - 1; i >= 0 && text[i] == '\n'; i-- {
		count++
	}
	if count == len(text) {
		r.trailingNewlines += count
		return
	}
	r.trailingNewlines = count
}

// block renders one node's children as document flow.
func (r *htmlRenderer) block(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		r.text(n.Data)
		return
	case html.DocumentNode:
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			r.block(child)
		}
		return
	case html.ElementNode:
	default:
		return
	}
	if skippedElements[n.DataAtom] || hiddenElement(n) {
		return
	}

	switch n.DataAtom {
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		// The atom constants are hashes, not a sequence, so the level cannot be
		// derived by subtraction: atom.H2 - atom.H1 is 263,424, not 1. That
		// mistake turned one <h2> into a quarter of a megabyte of '#'.
		level := map[atom.Atom]int{
			atom.H1: 1, atom.H2: 2, atom.H3: 3,
			atom.H4: 4, atom.H5: 5, atom.H6: 6,
		}[n.DataAtom]
		r.blankLines(2)
		if r.plain {
			r.inlineChildren(n)
		} else {
			r.write(strings.Repeat("#", level) + " ")
			r.inlineChildren(n)
		}
		r.blankLines(2)
	case atom.P:
		r.blankLines(2)
		r.inlineChildren(n)
		r.blankLines(2)
	case atom.Ul, atom.Ol:
		r.list(n)
	case atom.Li:
		r.listItem(n)
	case atom.Blockquote:
		r.quote(n)
	case atom.Pre:
		r.codeBlock(n)
	case atom.Hr:
		r.blankLines(2)
		if !r.plain {
			r.write("---")
		}
		r.blankLines(2)
	case atom.Table:
		r.table(n)
	case atom.Div, atom.Section, atom.Main, atom.Article, atom.Body, atom.Header,
		atom.Figure, atom.Figcaption, atom.Dl, atom.Dt, atom.Dd, atom.Details,
		atom.Summary, atom.Fieldset, atom.Label:
		r.blockChildren(n)
		// A div is a block in HTML, and a row of them is a row of separate
		// things. Without this, a filter bar whose labels are spans in sibling
		// divs reads as "GeneralArbitrationImmunefi Standard".
		if r.trailingNewlines == 0 && strings.TrimSpace(nodeText(n)) != "" {
			r.write("\n")
		}
	case atom.Br:
		r.write("\n")
	default:
		r.inlineChildren(n)
	}
}

// blockChildren walks children in document order, letting each decide its own
// spacing. It is what containers use so an inline wrapper does not invent a
// paragraph break.
func (r *htmlRenderer) blockChildren(n *html.Node) {
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode && isBlockElement(child) {
			r.block(child)
			continue
		}
		r.inlineNode(child)
	}
}

// inlineNode renders one inline element, including its own markers. Descending
// only through inlineChildren would drop the bold or link wrapper a container
// held directly.
func (r *htmlRenderer) inlineNode(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		r.text(n.Data)
		return
	case html.ElementNode:
	default:
		return
	}
	if skippedElements[n.DataAtom] || hiddenElement(n) {
		return
	}
	switch n.DataAtom {
	case atom.A:
		r.link(n)
	case atom.Img:
		r.image(n)
	case atom.Br:
		r.write("\n")
	case atom.Code:
		r.inlineCode(n)
	case atom.Strong, atom.B:
		r.wrap(n, "**")
	case atom.Em, atom.I:
		r.wrap(n, "*")
	case atom.Del, atom.S:
		r.wrap(n, "~~")
	default:
		r.inlineChildren(n)
	}
}

func isBlockElement(n *html.Node) bool {
	switch n.DataAtom {
	case atom.P, atom.Div, atom.Section, atom.Ul, atom.Ol, atom.Li, atom.H1,
		atom.H2, atom.H3, atom.H4, atom.H5, atom.H6, atom.Pre, atom.Blockquote,
		atom.Table, atom.Hr, atom.Main, atom.Article, atom.Header, atom.Footer,
		atom.Figure, atom.Dl, atom.Details, atom.Summary, atom.Form, atom.Nav,
		atom.Aside:
		return true
	}
	return false
}

// inlineChildren renders only inline content: text, links, emphasis, code.
func (r *htmlRenderer) inlineChildren(n *html.Node) {
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		r.inlineNode(child)
	}
}

func (r *htmlRenderer) wrap(n *html.Node, marker string) {
	if r.plain {
		r.inlineChildren(n)
		return
	}
	r.write(marker)
	r.inlineChildren(n)
	r.write(marker)
}

func (r *htmlRenderer) text(raw string) {
	if r.pre > 0 {
		r.write(raw)
		return
	}
	r.write(collapseSpaces(raw))
}

func (r *htmlRenderer) link(n *html.Node) {
	label := strings.TrimSpace(collapseSpaces(nodeText(n)))
	href := attribute(n, "href")
	if label == "" {
		return
	}
	if r.plain {
		r.write(label)
		return
	}
	resolved := r.resolve(href)
	switch {
	case resolved == "":
		r.write(label)
	case label == resolved:
		r.write("<" + resolved + ">")
	default:
		r.write("[" + label + "](" + resolved + ")")
	}
}

func (r *htmlRenderer) image(n *html.Node) {
	if r.plain {
		if alt := strings.TrimSpace(attribute(n, "alt")); alt != "" {
			r.write(alt)
		}
		return
	}
	alt := strings.TrimSpace(attribute(n, "alt"))
	src := r.resolve(attribute(n, "src"))
	if src == "" {
		return
	}
	r.write("![" + alt + "](" + src + ")")
}

func (r *htmlRenderer) inlineCode(n *html.Node) {
	text := strings.TrimSpace(collapseSpaces(nodeText(n)))
	if text == "" {
		return
	}
	if r.plain {
		r.write(text)
		return
	}
	r.write("`" + text + "`")
}

func (r *htmlRenderer) codeBlock(n *html.Node) {
	r.blankLines(2)
	language := ""
	if code := findFirst(n, atom.Code); code != nil {
		for _, class := range strings.Fields(attribute(code, "class")) {
			if strings.HasPrefix(class, "language-") {
				language = strings.TrimPrefix(class, "language-")
			}
		}
	}
	text := strings.Trim(nodeText(n), "\n")
	if r.plain {
		r.write(text)
	} else {
		r.write("```" + language + "\n" + text + "\n```")
	}
	r.blankLines(2)
}

func (r *htmlRenderer) list(n *html.Node) {
	r.blankLines(2)
	r.depth++
	ordered := n.DataAtom == atom.Ol
	index := 0
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if child.Type != html.ElementNode || child.DataAtom != atom.Li {
			continue
		}
		index++
		r.listItemWith(child, ordered, index)
	}
	r.depth--
	r.blankLines(2)
}

func (r *htmlRenderer) listItem(n *html.Node) {
	r.listItemWith(n, false, 0)
}

func (r *htmlRenderer) listItemWith(n *html.Node, ordered bool, index int) {
	if hiddenElement(n) {
		return
	}
	marker := "- "
	if ordered {
		marker = fmt.Sprintf("%d. ", index)
	}
	indent := strings.Repeat("  ", maxInt(r.depth-1, 0))
	r.write("\n" + indent + marker)
	hasBlockChild := false
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode && (child.DataAtom == atom.Ul || child.DataAtom == atom.Ol || child.DataAtom == atom.P) {
			hasBlockChild = true
			if child.DataAtom == atom.P {
				r.inlineChildren(child)
				continue
			}
			r.list(child)
			continue
		}
		if child.Type == html.ElementNode {
			if skippedElements[child.DataAtom] || hiddenElement(child) {
				continue
			}
			if isBlockElement(child) {
				r.block(child)
				continue
			}
			r.inlineNode(child)
			continue
		}
		if child.Type == html.TextNode {
			r.text(child.Data)
		}
	}
	if hasBlockChild {
		r.blankLines(1)
	}
}

func (r *htmlRenderer) quote(n *html.Node) {
	temp := &htmlRenderer{base: r.base, plain: r.plain, pre: r.pre}
	temp.blockChildren(n)
	text := collapseBlankLines(strings.TrimSpace(temp.sb.String()))
	r.blankLines(2)
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			r.write(">\n")
			continue
		}
		r.write("> " + line + "\n")
	}
	r.blankLines(1)
}

func (r *htmlRenderer) table(n *html.Node) {
	rows := findAll(n, atom.Tr)
	if len(rows) == 0 {
		r.blockChildren(n)
		return
	}
	r.blankLines(2)
	for index, row := range rows {
		cells := make([]string, 0, 4)
		for child := row.FirstChild; child != nil; child = child.NextSibling {
			if child.Type != html.ElementNode {
				continue
			}
			if child.DataAtom != atom.Td && child.DataAtom != atom.Th {
				continue
			}
			cells = append(cells, strings.TrimSpace(collapseSpaces(nodeText(child))))
		}
		if len(cells) == 0 {
			continue
		}
		r.write("| " + strings.Join(cells, " | ") + " |\n")
		if index == 0 && !r.plain {
			separators := make([]string, len(cells))
			for i := range separators {
				separators[i] = "---"
			}
			r.write("| " + strings.Join(separators, " | ") + " |\n")
		}
	}
	r.blankLines(2)
}

// blankLines ensures at least count newlines of separation without stacking.
func (r *htmlRenderer) blankLines(count int) {
	if r.trailingNewlines >= count {
		return
	}
	r.write(strings.Repeat("\n", count-r.trailingNewlines))
}

func (r *htmlRenderer) resolve(href string) string {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(strings.ToLower(href), "javascript:") ||
		strings.HasPrefix(href, "#") {
		return ""
	}
	parsed, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if r.base != nil {
		parsed = r.base.ResolveReference(parsed)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" && parsed.Scheme != "mailto" {
		return ""
	}
	return parsed.String()
}

func attribute(n *html.Node, key string) string {
	for _, attr := range n.Attr {
		if strings.EqualFold(attr.Key, key) {
			return attr.Val
		}
	}
	return ""
}

func hiddenElement(n *html.Node) bool {
	for _, attr := range n.Attr {
		switch strings.ToLower(attr.Key) {
		case "hidden":
			return true
		case "aria-hidden":
			if strings.EqualFold(strings.TrimSpace(attr.Val), "true") {
				return true
			}
		case "style":
			style := strings.ToLower(strings.ReplaceAll(attr.Val, " ", ""))
			if strings.Contains(style, "display:none") || strings.Contains(style, "visibility:hidden") {
				return true
			}
		}
	}
	return false
}

// collapseSpaces rewrites every run of whitespace inside a text node as one
// space. It runs on the node, not the document, so preformatted text keeps its
// own layout.
func collapseSpaces(text string) string {
	if text == "" {
		return ""
	}
	var builder strings.Builder
	builder.Grow(len(text))
	lastSpace := false
	for _, runeValue := range text {
		if unicode.IsSpace(runeValue) {
			if !lastSpace {
				builder.WriteRune(' ')
				lastSpace = true
			}
			continue
		}
		builder.WriteRune(runeValue)
		lastSpace = false
	}
	return builder.String()
}

func collapseBlankLines(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t")
		if strings.TrimSpace(trimmed) == "" {
			blank++
			if blank > 1 {
				continue
			}
			out = append(out, "")
			continue
		}
		blank = 0
		out = append(out, trimmed)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
