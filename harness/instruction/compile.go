package instruction

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaxInstructionBytes bounds one rule.
//
// A rule is a line in a tool result, and a tool result is re-sent on every later
// request in the session, so an unbounded rule is not one expensive turn but a
// recurring one. 480 bytes is about one dense sentence of policy: long enough to
// carry the rule and the reason it exists, short enough that four of them still
// read as a list rather than a wall.
//
// The cut is at a rune boundary and never at a sentence boundary. Cutting at a
// sentence would deliver the first half of a two-sentence rule, and half a rule
// reads as a whole rule: the model has no way to know the qualification was
// dropped, which makes that truncation worse than none at all.
const MaxInstructionBytes = 480

// VolumeWarn is the rule count above which a file is reported as carrying more
// rules than one file should.
//
// A warning and not a cap, because the honest remedy for a file that is too long
// is a smaller file, and a compiler that quietly drops the tail is the
// swallowed-tail failure this package exists to avoid. The number sits where a
// per-operation note stops being a reminder and becomes the prompt block it was
// meant to replace.
const VolumeWarn = 400

// imperativePrefixes is the standing-directive vocabulary. It was restated from
// a classifier that lived beside the deleted memory store; with the store gone
// this list is the only copy, and a directive vocabulary that disagrees with
// itself is how a rule fires for one consumer and silently not for another.
var imperativePrefixes = []string{
	"never ", "always ", "do not ", "don't ", "must ", "avoid ", "stop ",
}

// isImperative reports whether a statement is phrased as a standing directive.
//
// Anywhere in the statement, not only at the front, for the reason the original
// records: "for this task, always run the gate" is a one-off phrase followed by a
// standing rule, and the rule is the part that decides how it binds.
//
// The comparison is case-folded. Rules in a document do not start lower case,
// and this repository's own AGENTS.md opens bullets with "Never", "Do not" and
// "Always" throughout. A case-sensitive test reads every one of them as
// ordinary advice, which is the classification those sentences most obviously
// do not want.
func isImperative(text string) bool {
	folded := strings.ToLower(text)
	trimmed := strings.TrimSpace(folded)
	for _, prefix := range imperativePrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
		if strings.Contains(trimmed, ", "+prefix) {
			return true
		}
	}
	return false
}

// tagPattern matches the metadata trailers a rule may carry.
//
// `=` and never `:` because a colon is ordinary prose. "- push after every fix:
// ask first" is a sentence, and a parser that read that colon as a key would
// swallow the rest of it.
//
// The value class is deliberately narrow. Anything outside these shapes stays in
// the text, because a bracketed span the author wrote for the reader - a link
// label, a flag, a cross-reference - has to survive into the delivered rule
// rather than disappear into a parse.
var tagPattern = regexp.MustCompile(`\[(mandatory|advisory|explanatory|applies=[^\]\s]*|triggers=[^\]]*|tools=[^\]]*)\]`)

// stripTags removes metadata trailers from a rule and reports what they said.
//
// Removal happens before normalization and before hashing, which is what makes
// widening a rule's scope a non-event: "[mandatory] run the gate" and "[mandatory]
// run the gate [applies=harness/*]" are one sentence with one id, so a delivered
// record from before the widening still refers to the rule it was recorded for.
func stripTags(text string) (string, Priority, string, []string) {
	priority := Priority("")
	applies := ""
	triggers := []string{}

	out := tagPattern.ReplaceAllStringFunc(text, func(match string) string {
		body := match[1 : len(match)-1]
		switch {
		case body == string(Mandatory), body == string(Advisory), body == string(Explanatory):
			// Last one wins on a repeated tag, which needs no special case: the
			// later trailer is the one a reader applying the file top to bottom
			// meets last.
			priority = Priority(body)
		case strings.HasPrefix(body, "applies="):
			applies = strings.TrimSpace(strings.TrimPrefix(body, "applies="))
		case strings.HasPrefix(body, "triggers="):
			triggers = append(triggers, splitList(strings.TrimPrefix(body, "triggers="))...)
		case strings.HasPrefix(body, "tools="):
			// A tool is a trigger, not a scope. Folding it into Applies would put
			// two grammars in one field where neither is explained.
			for _, name := range splitList(strings.TrimPrefix(body, "tools=")) {
				triggers = append(triggers, "tool:"+name)
			}
		}
		return ""
	})

	return out, priority, applies, triggers
}

// splitList splits a comma-separated tag value, dropping empty entries.
func splitList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// normalize is what an identity is computed over: tags already stripped,
// whitespace runs folded to one space, ends trimmed.
//
// The folding restates memory.compact (harness/memory/store.go) for the reason
// every package that keeps a wording identity restates it locally: a rule
// rewrapped by an editor, reflowed to a different column width, or joined from a
// bullet and its continuation lines must not become a different rule.
//
// It is deliberately not a lower-caser, a punctuation stripper or a unicode
// normalizer. Those change words, and an id that survived "Never" becoming
// "never" would be an id that survived a change of emphasis the author did make.
func normalize(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// digest is the single hash both identities are rendered from.
//
// One digest, two renderings. If Hash were computed separately from ID the two
// could describe different rules, and the disagreement would be undetectable
// because both fields would still look well-formed.
func digest(normalized string) string {
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

// idFor renders the truncated digest as the rule's identity.
//
// Twelve bytes of sha256 is 24 hex characters, which with the "i" prefix is 25.
// Long enough that a collision across one project's rules is not a thing anyone
// meets, short enough to read in a rendered block and to hold per delivered rule
// for the life of a session.
func idFor(hash string) ID {
	return ID("i" + hash[:24])
}

// Compile turns one markdown instruction file into rules.
//
// It is pure and total: same inputs, same bytes out, every path returns. No
// clock, no filesystem, no package state. That is the property
// harness/verify.Decide holds and it is the same reason - a compiler you can read
// in one screen can be the definition of the policy rather than an
// implementation of it, and it can be tested exhaustively instead of by example.
//
// source is the project-relative path, carried for attribution and for pointing
// diagnostics at a file. Nothing here resolves it or reads it.
func Compile(source string, body []byte) (Snapshot, Diagnostics) {
	// CRLF is normalised before the walk rather than during it, so a file saved
	// on another platform compiles to the same digest and the same ids as the
	// file it was written from. That is the difference between "the author
	// reflowed the document" and "every rule in this project changed identity
	// because somebody opened it on the wrong machine".
	text := strings.ReplaceAll(string(body), "\r\n", "\n")
	w := &walker{
		source: source,
		lines:  strings.Split(text, "\n"),
		digest: digest(text),
		counts: map[ID]int{},
		origin: map[ID]int{},
	}
	for index, line := range w.lines {
		if strings.TrimSpace(line) != "" {
			w.last = index + 1
		}
	}
	w.run()
	return w.snapshot(), w.diags
}

// heading is one entry of the heading stack.
type heading struct {
	level int
	title string
}

// builder accumulates the lines of one rule before it is finished.
type builder struct {
	lines []string
	// line is where the rule opened and endLine where its last line was.
	line    int
	endLine int
	// section is the heading path captured when the rule opened, so a heading
	// later in the file cannot re-parent a rule that is already open.
	section string
	// column is the content column of the bullet that opened the rule, and is
	// what a continuation line must reach to join it. Zero for a paragraph,
	// which continues on any non-blank line.
	column int
	// bullet records that the rule came from a list item, which decides the
	// default priority when the author tagged nothing.
	bullet bool
}

// empty reports whether the builder holds no words at all, which is what a bare
// bullet marker on its own line looks like.
func (b *builder) empty() bool {
	for _, line := range b.lines {
		if strings.TrimSpace(line) != "" {
			return false
		}
	}
	return true
}

// walker holds the compile's state for one file.
type walker struct {
	source string
	lines  []string
	// digest is of the CRLF-normalised source text, so a store can compare the
	// bytes it read against the bytes that produced this snapshot.
	digest string
	// last is the highest line number carrying content, which is the far end of
	// a swallowed tail.
	last    int
	fence   string
	fenceAt int
	comment bool
	// commentAt is where an unclosed comment opened.
	commentAt int
	// headings is the open heading stack, outermost first, and is what the
	// section path is rendered from.
	headings   []heading
	open       *builder
	tableAt    int
	tableEnd   int
	tableDelim bool
	out        []Instruction
	diags      Diagnostics
	// counts is how many copies of each base id have been emitted and origin is
	// where the first one was, so a repeat can be suffixed in one pass.
	counts map[ID]int
	origin map[ID]int
}

// section is the heading path the current rule inherits.
func (w *walker) section() string { return w.sectionPath() }

func (w *walker) snapshot() Snapshot {
	return Snapshot{Source: w.source, Digest: w.digest, Instructions: w.out}
}

func (w *walker) diag(kind DiagnosticKind, line, endLine int, message string) {
	if endLine < line {
		endLine = line
	}
	w.diags = append(w.diags, Diagnostic{
		Kind:    kind,
		Source:  w.source,
		Line:    line,
		EndLine: endLine,
		Message: message,
	})
}

// run walks every line.
func (w *walker) run() {
	for index, raw := range w.lines {
		w.step(index+1, raw)
	}
	w.finish()
}

// finish reports what the file ended inside, then flushes and lints.
//
// Every check here is one question asked of three constructs: did the file stop
// while something was open, and if so how much did that consume? A construct
// that swallows the tail and says nothing is the failure this whole file guards
// against, because the result still looks like a compiler that worked.
func (w *walker) finish() {
	if w.fence != "" {
		w.diag(DiagUnclosedFence, w.fenceAt, w.last,
			"the code fence opened here was never closed, so every line after it was read as code and produced no rules")
	}
	if w.comment {
		w.diag(DiagUnclosedComment, w.commentAt, w.last,
			"the HTML comment opened here was never closed, so every line after it was read as comment and produced no rules")
	}
	if w.tableAt != 0 && !w.tableDelim {
		w.diag(DiagUnterminatedTable, w.tableAt, w.tableEnd,
			"the rows starting here have no header delimiter, so they were read as reference data and dropped rather than delivered as rules")
	}
	w.flush()
	w.lint()
}

// lint reviews the compiled result.
//
// It runs on the output rather than on the text because what it can complain
// about - a scope that matches everything, more rules than one file should carry
// - is only visible once the rules exist.
func (w *walker) lint() {
	if len(w.out) > VolumeWarn {
		w.diag(DiagVolume, 1, w.last,
			"the file compiled to "+strconv.Itoa(len(w.out))+" rules, over the "+strconv.Itoa(VolumeWarn)+
				" at which a per-operation note stops being a reminder")
	}
	for _, item := range w.out {
		if item.Applies != "" && !strings.ContainsAny(item.Applies, "/.") {
			w.diag(DiagBroadScope, item.Line, item.EndLine,
				"applies="+item.Applies+" carries neither \"/\" nor \".\", so it matches that base name anywhere in the tree")
		}
	}
	w.lintDuplicates()
}

// lintDuplicates reports one finding per repeated sentence, not one per copy.
//
// The count matters: three copies of one sentence is one thing wrong with the
// document, and a caller reading a log of three identical warnings learns
// nothing it did not learn from the first one.
func (w *walker) lintDuplicates() {
	type group struct {
		first  ID
		count  int
		origin int
		last   int
	}
	groups := map[ID]*group{}
	var order []ID
	for _, item := range w.out {
		if item.DuplicateOf == "" {
			continue
		}
		g, ok := groups[item.DuplicateOf]
		if !ok {
			g = &group{first: item.DuplicateOf, origin: item.Line}
			groups[item.DuplicateOf] = g
			order = append(order, item.DuplicateOf)
		}
		g.count++
		g.last = item.EndLine
	}
	for _, key := range order {
		g := groups[key]
		// The origin is where the first copy was, not where the first repeat was.
		// "This text appears three times, starting at line 7" sends the reader
		// to the copy they have already seen rather than to the one that has to
		// be deleted.
		first := g.origin
		if line, ok := w.origin[key]; ok {
			first = line
		}
		w.diag(DiagDuplicate, first, g.last,
			"identical text appears "+strconv.Itoa(g.count+1)+" times in this file (first at line "+
				strconv.Itoa(first)+"); every copy is kept as a rule and the repeats are id-suffixed from "+string(key))
	}
}

// step classifies one line and applies it.
//
// The order is not a preference. A fence outranks everything, because the line
// after an opening fence is the inside of an example and reads exactly like a
// rule. A table row outranks a paragraph, because "|" is data. A heading
// outranks a bullet, because "## Module" is scope rather than instruction and
// delivering it as a rule would put the table of contents into every note.
//
// A nested bullet is a rule of its own rather than a continuation of the bullet
// above it. Its marker starts a new item, and joining the two would produce one
// sentence with two subjects - "Never manage the daemon this way: use ceraa start"
// - where the sub-item is frequently a different rule that merely lives under a
// heading the author did not have. The parent section still reaches the reader
// through Section, which is what the nesting was carrying.
func (w *walker) step(number int, raw string) {
	line := stripQuotes(raw)

	switch {
	case w.comment:
		w.stepComment(number, line)
		return
	case w.fence != "":
		// A closing fence must be the same character and at least as long, which
		// is what lets a document show a fence inside a fence.
		if marker := fenceMarker(line); marker != "" && sameFence(marker, w.fence) {
			w.fence = ""
		}
		return
	}

	if marker := fenceMarker(line); marker != "" {
		w.closeTable(number)
		w.flush()
		w.fence = marker
		w.fenceAt = number
		return
	}
	if strings.HasPrefix(strings.TrimSpace(line), "<!--") {
		w.stepComment(number, line)
		return
	}

	trimmed := strings.TrimSpace(line)
	switch {
	case trimmed == "":
		// A blank line ends a table run and then a rule. The table closes first
		// because a rule left open across a table would absorb table rows as
		// continuation lines.
		w.closeTable(number)
		w.flush()
		return
	case isHeading(trimmed):
		w.closeTable(number)
		w.flush()
		w.pushHeading(headingText(trimmed))
		return
	case strings.HasPrefix(trimmed, "|"):
		w.stepTable(number, trimmed)
		return
	case isRuleLine(trimmed):
		w.closeTable(number)
		w.flush()
		return
	}

	w.closeTable(number)
	if column, content, ok := bulletColumn(line); ok {
		w.flush()
		w.open = &builder{line: number, section: w.section(), column: column, bullet: true}
		w.append(number, content)
		return
	}
	if w.open != nil && indentedTo(line, w.open.column) {
		w.append(number, trimmed)
		return
	}
	w.start(number, trimmed)
}

// stepComment consumes a line while inside an HTML comment, opening one when the
// line was not already inside one.
func (w *walker) stepComment(number int, line string) {
	if !w.comment {
		w.comment = true
		w.commentAt = number
	}
	if strings.Contains(line, "-->") {
		w.comment = false
	}
}

// stepTable consumes a table row.
//
// A run of "|" lines is reference data only once a delimiter row has shown it to
// be a table. Without one it is ambiguous prose, and the safe reading is to drop
// it and say so rather than deliver a colour swatch as a rule.
func (w *walker) stepTable(number int, trimmed string) {
	if w.tableAt == 0 {
		w.flush()
		w.tableAt = number
		w.tableEnd = number
		w.tableDelim = false
	}
	w.tableEnd = number
	if isTableDelimiter(trimmed) {
		w.tableDelim = true
	}
}

// closeTable ends a table run at the line that ended it, reporting the rows that
// could not be identified as a table.
//
// The far end is the last row rather than the line that closed the run, because
// the finding is about the rows and a reader who is sent one line past them has
// to work out which half of the report covers the mistake.
func (w *walker) closeTable(number int) {
	if w.tableAt == 0 {
		return
	}
	if !w.tableDelim {
		w.diag(DiagUnterminatedTable, w.tableAt, w.tableEnd,
			"the rows starting here have no header delimiter, so they were read as reference data and dropped rather than delivered as rules")
	}
	w.tableAt = 0
	w.tableEnd = 0
	w.tableDelim = false
}

// pushHeading maintains the heading stack and the section path.
func (w *walker) pushHeading(next heading) {
	for len(w.headings) > 0 && w.headings[len(w.headings)-1].level >= next.level {
		w.headings = w.headings[:len(w.headings)-1]
	}
	w.headings = append(w.headings, next)
}

func (w *walker) sectionPath() string {
	titles := make([]string, 0, len(w.headings))
	for _, item := range w.headings {
		titles = append(titles, item.title)
	}
	return strings.Join(titles, " > ")
}

// start opens a new paragraph rule.
//
// A paragraph start always closes the previous rule. Markdown would let a lazy
// continuation carry a paragraph across, but two rules merged into one sentence
// is exactly what this segmentation exists to prevent, and the blank line a
// human writes between them is the only signal there is.
func (w *walker) start(number int, text string) {
	w.flush()
	w.open = &builder{line: number, section: w.section()}
	w.append(number, text)
}

// append adds a line to the open rule, opening one if none is open.
func (w *walker) append(number int, text string) {
	if w.open == nil {
		w.open = &builder{line: number, section: w.section()}
	}
	w.open.lines = append(w.open.lines, text)
	w.open.endLine = number
}

// flush turns the open lines into a rule, if they hold any words.
//
// Nothing errors here: a rule with no words is a bullet marker alone on its
// line, which markdown allows and which means nothing to say.
func (w *walker) flush() {
	if w.open == nil || w.open.empty() {
		w.open = nil
		return
	}
	w.emit(*w.open)
	w.open = nil
}

// emit compiles one finished rule.
//
// The order is fixed and each step is a precondition of the next: tags off,
// whitespace folded, cap applied, digest taken, id rendered, repeats suffixed.
// The digest is taken before the repeat suffix on purpose, so an id does not
// depend on how many copies of a sentence happened to exist when it was read -
// adding a second copy above a rule leaves every id below it alone.
func (w *walker) emit(b builder) {
	joined := strings.Join(b.lines, " ")
	stripped, tagged, applies, triggers := stripTags(joined)
	text := normalize(stripped)
	if text == "" {
		return
	}

	priority := tagged
	if priority == "" {
		priority = defaultPriority(text, b.bullet)
	}

	endLine := b.endLine
	if endLine < b.line {
		endLine = b.line
	}
	oversized := false
	if len(text) > MaxInstructionBytes {
		text = truncateRunes(text, MaxInstructionBytes)
		oversized = true
	}

	hash := digest(text)
	base := idFor(hash)
	item := Instruction{
		ID:        base,
		Text:      text,
		Source:    w.source,
		Line:      b.line,
		EndLine:   endLine,
		Section:   b.section,
		Applies:   applies,
		Triggers:  triggers,
		Priority:  priority,
		Hash:      hash,
		Oversized: oversized,
	}
	// Two identical sentences are two rules in the document, and both are kept:
	// a repetition is a fact about the document, and collapsing it would make
	// the compiled rule count a lie.
	//
	// The first copy in document order keeps the unsuffixed id. An id already
	// written into a delivered set refers to the copy a reader met first, and
	// making that the suffixed one would invalidate every record of it on the
	// next compile.
	if copies := w.counts[base]; copies == 0 {
		w.origin[base] = b.line
	} else {
		item.DuplicateOf = base
		item.ID = ID(string(base) + strconv.Itoa(copies))
	}
	w.counts[base]++
	w.out = append(w.out, item)
}

// defaultPriority classifies an untagged rule by its shape.
//
// The asymmetry is deliberate and is the whole reason paragraphs are compiled at
// all rather than skipped: in a document like AGENTS.md a bullet is nearly
// always a rule and a paragraph is nearly always the reason for one. Both are
// raised by wording and never lowered by it, so a directive written as prose
// still reads as a directive.
func defaultPriority(text string, bullet bool) Priority {
	switch {
	case bullet && isImperative(text):
		return Mandatory
	case bullet:
		return Advisory
	case isImperative(text):
		return Advisory
	default:
		return Explanatory
	}
}

// truncateRunes cuts to a byte budget on a rune boundary.
func truncateRunes(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.ValidString(text[:cut]) {
		cut--
	}
	return strings.TrimRight(text[:cut], " ")
}

// bulletPattern matches a list marker: "-", "*", "+", "12." or "12)" followed
// by space or end of line.
//
// End of line is included because a bullet whose content starts on the next line
// is still a bullet, and treating the bare marker as a paragraph is how a wrapped
// list item becomes two rules.
var bulletPattern = regexp.MustCompile(`^(?:[-*+]|\d{1,9}[.)])(?:[ \t]+|$)`)

// bulletColumn returns the content column of a list marker, its content, and
// whether the line is a bullet at all.
func bulletColumn(line string) (int, string, bool) {
	trimmed, indent := trimIndent(line)
	if !bulletPattern.MatchString(trimmed) {
		return 0, "", false
	}
	width := markerWidth(trimmed)
	rest := trimmed[width:]
	spaces := 0
	for _, r := range rest {
		if r != ' ' && r != '\t' {
			break
		}
		spaces++
	}
	content := strings.TrimSpace(rest[spaces:])
	if content == "" {
		// A bare marker opens a rule with nothing in it yet, so its content
		// column is one past the marker and its continuation lines are indented
		// to at least that.
		return indent + width, "", true
	}
	return indent + width + spaces, content, true
}

// markerWidth is the rune width of a bullet marker.
func markerWidth(line string) int {
	if line == "" {
		return 0
	}
	if line[0] == '-' || line[0] == '*' || line[0] == '+' {
		return 1
	}
	width := 0
	for width < len(line) && line[width] >= '0' && line[width] <= '9' {
		width++
	}
	return width + 1
}

// trimIndent removes leading whitespace and reports its width in runes.
//
// Runes rather than bytes because a bullet marker is ASCII while the content
// after it may not be, and a continuation test that counted bytes would join one
// rule here and start another there depending on the words.
func trimIndent(line string) (string, int) {
	width := 0
	for _, r := range line {
		if r != ' ' && r != '\t' {
			break
		}
		width++
	}
	return line[width:], width
}

// indentedTo reports whether a line reaches a content column.
func indentedTo(line string, column int) bool {
	if column <= 0 {
		return true
	}
	_, width := trimIndent(line)
	return width >= column
}

// stripQuotes removes every blockquote level from a line.
//
// All of them, not one per pass, because a quoted bullet is still a rule and
// half-stripped ">> never do this" would arrive as the prose "> never do this".
func stripQuotes(line string) string {
	for {
		trimmed, _ := trimIndent(line)
		if !strings.HasPrefix(trimmed, ">") {
			return line
		}
		trimmed = strings.TrimPrefix(trimmed, ">")
		trimmed = strings.TrimPrefix(trimmed, " ")
		line = trimmed
	}
}

// fenceMarker returns the fence run that opens or closes a block, or "".
//
// The whole run is returned rather than the first character, because a
// four-backtick fence is the legitimate way to show a three-backtick fence and a
// shorter run must not end it.
func fenceMarker(line string) string {
	trimmed, width := trimIndent(line)
	if width > 3 {
		// CommonMark gives indented code four spaces of its own, and a fence
		// inside it is code, not a fence.
		return ""
	}
	if !strings.HasPrefix(trimmed, "```") && !strings.HasPrefix(trimmed, "~~~") {
		return ""
	}
	marker := trimmed[0]
	run := 0
	for run < len(trimmed) && trimmed[run] == marker {
		run++
	}
	return strings.Repeat(trimmed[:1], run)
}

// sameFence reports whether a marker closes an open fence: same character, at
// least as long.
func sameFence(marker, open string) bool {
	return marker[0] == open[0] && len(marker) >= len(open)
}

// isHeading reports whether a trimmed line is an ATX heading: one to six hashes
// followed by a space.
//
// Seven hashes is not a heading, it is a fence in disguise, and a document using
// one deserves to have it delivered as the prose it is rather than silently
// promoted into a section title.
func isHeading(trimmed string) bool {
	if !strings.HasPrefix(trimmed, "#") {
		return false
	}
	level := 0
	for level < len(trimmed) && trimmed[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level >= len(trimmed) {
		return false
	}
	return trimmed[level] == ' '
}

// headingText strips the hashes and any closing run of them.
//
// The closing run goes because "# Module ##" and "# Module" are the same heading
// written by a generator and by a person, and a section path that differs
// between them makes the same rule look relocated.
func headingText(trimmed string) heading {
	level := 0
	for level < len(trimmed) && trimmed[level] == '#' {
		level++
	}
	body := strings.TrimSpace(trimmed[level:])
	body = strings.TrimSpace(strings.TrimRight(body, "#"))
	return heading{level: level, title: body}
}

// isRuleLine reports whether a trimmed line is a horizontal rule.
//
// Checked before a bullet because "---" is both, and a document divider
// delivered to the model as the rule "---" is noise in every note.
func isRuleLine(trimmed string) bool {
	if len(trimmed) < 3 {
		return false
	}
	for _, marker := range []string{"-", "*", "_"} {
		if strings.Trim(trimmed, marker) == "" {
			return true
		}
	}
	return false
}

// isTableDelimiter reports whether a row is a table's header separator.
//
// It has to be true for a run to count as reference data at all, which is what
// licenses dropping the rows around it.
func isTableDelimiter(trimmed string) bool {
	if !strings.Contains(trimmed, "-") {
		return false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(trimmed, "|"), "|")
	cells := strings.Split(inner, "|")
	for _, cell := range cells {
		cell = strings.TrimSpace(cell)
		cell = strings.Trim(cell, ":")
		if cell == "" || strings.Trim(cell, "-") != "" {
			return false
		}
	}
	return true
}
