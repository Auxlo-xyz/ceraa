package coordinator

import "strings"

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

// refusalPhrases are the opening forms that count as a refusal. Declining and
// being unable are the same failure mode here: both stop the turn before any
// investigation, so both get the nudge.
var refusalPhrases = []string{
	"i can't",
	"i cannot",
	"i can not",
	"i'm unable",
	"i am unable",
	"i'm not able",
	"i am not able",
	"i won't be able",
	"i will not be able",
}

// refusalIdioms contain the phrases above without being refusals. Removed
// before matching so enthusiasm is not answered with a lecture about effort.
var refusalIdioms = []string{
	"can't wait",
	"cannot wait",
	"can't believe",
	"cannot believe",
	"can't recommend",
	"cannot recommend",
}

// looksLikeRefusal reports whether the opening of an answer refuses the task.
func looksLikeRefusal(text string) bool {
	words := strings.Fields(strings.ToLower(normalizeApostrophes(text)))
	if len(words) > refusalWindow {
		words = words[:refusalWindow]
	}
	joined := " " + strings.Join(words, " ") + " "
	for _, idiom := range refusalIdioms {
		joined = strings.ReplaceAll(joined, " "+idiom, " ")
	}
	for _, phrase := range refusalPhrases {
		if strings.Contains(joined, phrase) {
			return true
		}
	}
	return false
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
