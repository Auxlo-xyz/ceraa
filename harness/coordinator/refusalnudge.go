package coordinator

import (
	"strings"
	"unicode"
)

// refusalNudgePrompt is injected as a turn when the assistant opens with a
// refusal. The wording is deliberately about evidence and effort rather than
// about permission: the failure it targets is a conclusion reached before any
// attempt, which a web search, an available tool or a cheaper approach would
// have overturned.
const refusalNudgePrompt = "Before reporting that something cannot be done, investigate whether a web search, the tools you already have, or a simpler and more efficient approach can solve the user's exact task. Explore the alternatives, verify what actually works, and make a genuine attempt before answering."

// refusalWindow is how many opening words are inspected. A refusal after this
// point is treated as a finding about work already attempted rather than a
// reflex, so it is left alone.
const refusalWindow = 25

// refusalVerbs are the forms that count as a refusal, as token sequences. A
// sequence rather than a phrase because the same refusal arrives with words in
// between: "I genuinely can't", "I honestly can't", "I really cannot". The
// subject ("i", "i'm", "im") is matched separately, before these.
//
// Declining and being unable are the same failure mode here: both stop the turn
// before any investigation, so both get the nudge.
var refusalVerbs = [][]string{
	{"can't"},
	{"cannot"},
	{"can", "not"},
	{"unable"},
	{"not", "able"},
	{"won't", "be", "able"},
	{"will", "not", "be", "able"},
	{"am", "unable"},
	{"am", "not", "able"},
}

// refusalFillers are dropped before matching so an adverb does not hide the
// verb. They carry no meaning for this decision.
var refusalFillers = map[string]bool{
	"genuinely": true, "honestly": true, "really": true, "truly": true,
	"actually": true, "simply": true, "just": true, "definitely": true,
	"certainly": true, "absolutely": true, "sincerely": true, "quite": true,
	"literally": true, "currently": true,
}

// refusalIdioms begin like a refusal but are enthusiasm or advice, so the verb
// match is discarded when the next word starts one of these.
var refusalIdioms = map[string]bool{
	"wait": true, "believe": true, "recommend": true,
}

// looksLikeRefusal reports whether the opening of an answer refuses the task.
func looksLikeRefusal(text string) bool {
	words := refusalTokens(text)
	if len(words) > refusalWindow {
		words = words[:refusalWindow]
	}
	filtered := make([]string, 0, len(words))
	for _, word := range words {
		if !refusalFillers[word] {
			filtered = append(filtered, word)
		}
	}
	for i := 0; i < len(filtered); i++ {
		if !isRefusalSubject(filtered[i]) {
			continue
		}
		if matchesRefusalVerb(filtered[i+1:]) {
			return true
		}
	}
	return false
}

func isRefusalSubject(word string) bool {
	return word == "i" || word == "i'm" || word == "im"
}

// matchesRefusalVerb reports whether the tokens after a subject open with one
// of the refusal forms.
func matchesRefusalVerb(rest []string) bool {
	for _, verb := range refusalVerbs {
		if len(rest) < len(verb) {
			continue
		}
		matched := true
		for i, want := range verb {
			if rest[i] != want {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		next := ""
		if len(rest) > len(verb) {
			next = rest[len(verb)]
		}
		if refusalIdioms[next] {
			continue
		}
		return true
	}
	return false
}

// refusalTokens lowercases, folds apostrophes and strips sentence punctuation,
// so "I can't," and "I can’t" compare as words. Apostrophes inside a word are
// kept: "can't" and "i'm" depend on them.
func refusalTokens(text string) []string {
	fields := strings.Fields(strings.ToLower(normalizeApostrophes(text)))
	words := make([]string, 0, len(fields))
	for _, field := range fields {
		word := strings.TrimFunc(field, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
		})
		if word != "" {
			words = append(words, word)
		}
	}
	return words
}

// normalizeApostrophes folds the typographic forms onto the ASCII one, because
// models emit both and only one of them matches a literal list.
func normalizeApostrophes(text string) string {
	return strings.NewReplacer("\u2019", "'", "\u2018", "'", "\u02bc", "'", "\u00b4", "'").Replace(text)
}

// refusalGate holds the opening of a stream until it is known whether the turn
// is a refusal. Nothing reaches the renderer while it holds, which is the whole
// point: once a refusal is on screen the user has read it, and correcting it
// afterwards reads as a contradiction rather than a retry.
type refusalGate struct {
	emit    func(string)
	buf     strings.Builder
	holding bool
	fired   bool
}

func newRefusalGate(emit func(string)) *refusalGate {
	return &refusalGate{emit: emit}
}

// hold starts buffering. Used while the nudge is still available for this run.
func (g *refusalGate) hold() { g.holding = true }

// observe feeds one content delta. It reports true when a refusal was found,
// at which point the caller aborts the request: the text was discarded and
// replaying it after the nudge would duplicate the turn.
func (g *refusalGate) observe(delta string) bool {
	if g.fired {
		return true
	}
	if !g.holding {
		g.emit(delta)
		return false
	}
	g.buf.WriteString(delta)
	if looksLikeRefusal(g.buf.String()) {
		g.fired = true
		g.buf.Reset()
		return true
	}
	// Past the window the opening is no longer a refusal, so the held text is
	// the answer and must be released before the rest of it arrives.
	if len(strings.Fields(g.buf.String())) > refusalWindow {
		g.release()
	}
	return false
}

// release stops holding and emits whatever was buffered. Called when the turn
// turns out not to be a refusal: a tool call proves the model is acting, and
// the end of the stream means the held text was the whole answer.
func (g *refusalGate) release() {
	if !g.holding {
		return
	}
	g.holding = false
	if g.buf.Len() > 0 {
		g.emit(g.buf.String())
		g.buf.Reset()
	}
}

// firedRefusal reports whether a refusal was found and discarded.
func (g *refusalGate) firedRefusal() bool { return g.fired }
