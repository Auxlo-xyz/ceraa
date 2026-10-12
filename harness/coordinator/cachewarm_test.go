package coordinator

import (
	"fmt"
	"strings"
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
)

func TestCacheSignatureIsLengthNotContent(t *testing.T) {
	a := []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "hello"}}
	b := []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "worlds"}}
	if cacheSignature(a) == cacheSignature(b) {
		t.Fatal("two different strings of different length signed the same")
	}
	// Same shape, different words: the signature is about whether anything
	// earlier in the conversation changed size, not about what it says.
	c := []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "world"}}
	if cacheSignature(a) != cacheSignature(c) {
		t.Fatal("equal-length replacements changed the signature")
	}
}

func TestCacheSignatureAppendsArePureExtensions(t *testing.T) {
	base := []llm.Item{
		{Kind: "text", Role: llm.RoleUser, Text: "one"},
		{Kind: "text", Role: llm.RoleAssistant, Text: "two"},
	}
	extended := append(append([]llm.Item(nil), base...),
		llm.Item{Kind: "text", Role: llm.RoleUser, Text: "three"})
	if got := firstShapeChange(cacheSignature(base), cacheSignature(extended)); got != "" {
		t.Fatalf("an appended turn reported %q, want no regression", got)
	}
}

// An append is the common case and must not read as a regression. The
// production log made this concrete: 657 of 742 warnings were appends, so the
// 85 real ones could not be told apart from ordinary conversation.
func TestAppendIsNotReportedAsARegression(t *testing.T) {
	base := cacheSignature([]llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "one"}})
	extended := cacheSignature([]llm.Item{
		{Kind: "text", Role: llm.RoleUser, Text: "one"},
		{Kind: "text", Role: llm.RoleAssistant, Text: "two"},
	})
	ch := classifyPrefixChange(base, extended)
	if ch.Regressed {
		t.Fatalf("an append reported %q as a regression", ch.Note)
	}
	if !strings.Contains(ch.Note, "appended") {
		t.Fatalf("note = %q, want it to name the append", ch.Note)
	}
}

// A shape that shrank past an identical prefix has the same property: every
// byte the provider holds is still present and still in the same order.
func TestTruncationToAnIdenticalPrefixIsNotARegression(t *testing.T) {
	long := cacheSignature([]llm.Item{
		{Kind: "text", Role: llm.RoleUser, Text: "one"},
		{Kind: "text", Role: llm.RoleAssistant, Text: "two"},
	})
	short := cacheSignature([]llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "one"}})
	if ch := classifyPrefixChange(long, short); ch.Regressed {
		t.Fatalf("truncating to an identical prefix reported %q", ch.Note)
	}
}

func TestFirstShapeChangeFindsThePosition(t *testing.T) {
	prev := cacheSignature([]llm.Item{
		{Kind: "text", Role: llm.RoleUser, Text: "one"},
		{Kind: "tool", Role: llm.RoleTool, Output: "result"},
	})
	cur := cacheSignature([]llm.Item{
		{Kind: "text", Role: llm.RoleUser, Text: "one"},
		{Kind: "tool", Role: llm.RoleTool, Output: "shorter"},
	})
	if got := firstShapeChange(prev, cur); got != "item1" {
		t.Fatalf("change = %q, want item1", got)
	}
}

func TestSessionSignatureChangesOnAppendAndRewrite(t *testing.T) {
	sess := &sessionstore.Session{ID: "s", Messages: []sessionstore.Message{
		{Role: llm.RoleUser, Content: "one"},
	}}
	first := sessionSignature(sess)
	sess.Messages = append(sess.Messages, sessionstore.Message{Role: llm.RoleAssistant, Content: "two"})
	if sessionSignature(sess) == first {
		t.Fatal("an appended message signed the same")
	}
	appended := sessionSignature(sess)
	sess.Messages[1].Content = "different"
	if sessionSignature(sess) == appended {
		t.Fatal("a rewritten newest message signed the same")
	}
	if got := sessionSignature(&sessionstore.Session{}); got != "empty" {
		t.Fatalf("empty session signed %q", got)
	}
}

func TestLogCachePrefixPersistsPerSession(t *testing.T) {
	sess := &sessionstore.Session{ID: "s"}
	items := []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "hello"}}
	logCachePrefix(sess, 0, items, nil, llm.TokenBreakdown{})
	if sess.PrefixSignature == "" {
		t.Fatal("the signature was not persisted")
	}
	if sess.PrefixSignatureEpoch != sess.PrefixRewrites {
		t.Fatal("the epoch was not recorded with the signature")
	}
	// A pure extension of the same conversation is not a regression.
	extended := append(append([]llm.Item(nil), items...),
		llm.Item{Kind: "text", Role: llm.RoleAssistant, Text: "hi"})
	logCachePrefix(sess, 0, extended, nil, llm.TokenBreakdown{})
	if sess.PrefixSignature == "" {
		t.Fatal("the signature was cleared by an appended turn")
	}
}

func TestLogCachePrefixRewriteIsExpected(t *testing.T) {
	sess := &sessionstore.Session{ID: "s", PrefixRewrites: 0}
	logCachePrefix(sess, 0, []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "hello"}}, nil, llm.TokenBreakdown{})
	// A fold rewrites the front of the history and bumps the epoch, so the
	// divergence it causes is the one the session is allowed to make.
	sess.PrefixRewrites++
	sess.Messages = append(sess.Messages, sessionstore.Message{Role: llm.RoleSystem, Content: "<conversation-checkpoint>"})
	logCachePrefix(sess, 0, []llm.Item{{Kind: "compaction", Role: llm.RoleSystem, Text: "<conversation-checkpoint>"}}, nil, llm.TokenBreakdown{})
	if sess.PrefixSignatureEpoch != sess.PrefixRewrites {
		t.Fatal("the epoch did not advance with the rewrite")
	}
}

// A change at the front of the history and a change at the end are not the same
// event, and the log used to call them by the same name. In the production log
// they were two hours apart: item46 of 171 left 140 cached tokens out of 110,000,
// and item170 of 173 left the cache at 99.4%. One warning cannot mean both.
func TestTailRewriteIsNotARegression(t *testing.T) {
	// 200 items, the first 195 identical. Three changed at the end.
	var prev, cur []llm.Item
	for i := 0; i < 195; i++ {
		prev = append(prev, llm.Item{Kind: "text", Role: llm.RoleUser, Text: fmt.Sprintf("m%d", i)})
		cur = append(cur, llm.Item{Kind: "text", Role: llm.RoleUser, Text: fmt.Sprintf("m%d", i)})
	}
	for i := 0; i < 5; i++ {
		prev = append(prev, llm.Item{Kind: "text", Role: llm.RoleAssistant, Text: fmt.Sprintf("old item %d", i)})
		cur = append(cur, llm.Item{Kind: "text", Role: llm.RoleAssistant, Text: fmt.Sprintf("rewritten item %d", i)})
	}
	ch := classifyPrefixChange(cacheSignature(prev), cacheSignature(cur))
	if !ch.Regressed {
		t.Fatalf("a changed item was not reported: %q", ch.Note)
	}
	if !ch.IsTailRewrite(len(cur)) {
		t.Fatalf("a %d-item tail change in a %d-item request was called a regression", ch.Rewritten, len(cur))
	}
}

// The other population: the change starts early and everything from there is
// re-priced. This is the case the warning exists for and it must stay loud.
func TestFrontChangeIsStillARegression(t *testing.T) {
	// 171 items, the first 46 identical. 125 items change from there onward.
	var prev, cur []llm.Item
	for i := 0; i < 46; i++ {
		prev = append(prev, llm.Item{Kind: "text", Role: llm.RoleUser, Text: fmt.Sprintf("m%d", i)})
		cur = append(cur, llm.Item{Kind: "text", Role: llm.RoleUser, Text: fmt.Sprintf("m%d", i)})
	}
	for i := 0; i < 125; i++ {
		prev = append(prev, llm.Item{Kind: "text", Role: llm.RoleAssistant, Text: fmt.Sprintf("old item %d", i)})
		cur = append(cur, llm.Item{Kind: "text", Role: llm.RoleAssistant, Text: fmt.Sprintf("rewritten item %d", i)})
	}
	ch := classifyPrefixChange(cacheSignature(prev), cacheSignature(cur))
	if !ch.Regressed {
		t.Fatalf("a changed item was not reported: %q", ch.Note)
	}
	if ch.IsTailRewrite(len(cur)) {
		t.Fatalf("a %d-item change from item%d was excused as a tail rewrite", ch.Rewritten, ch.At)
	}
	if ch.Rewritten != 125 {
		t.Fatalf("rewritten = %d, want 125", ch.Rewritten)
	}
}

// The fraction is taken against the request just sent, not against a constant
// count. Five changed items in a six-item request is most of the request and
// cannot be excused just because five is a small number.
func TestTailRewriteIsMeasuredAgainstTheRequest(t *testing.T) {
	prev := []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "kept"}}
	cur := []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "kept"}}
	for i := 0; i < 5; i++ {
		prev = append(prev, llm.Item{Kind: "text", Role: llm.RoleAssistant, Text: fmt.Sprintf("old item %d", i)})
		cur = append(cur, llm.Item{Kind: "text", Role: llm.RoleAssistant, Text: fmt.Sprintf("rewritten item %d", i)})
	}
	ch := classifyPrefixChange(cacheSignature(prev), cacheSignature(cur))
	if !ch.Regressed {
		t.Fatalf("a changed item was not reported: %q", ch.Note)
	}
	if ch.Rewritten != 5 {
		t.Fatalf("rewritten = %d, want 5", ch.Rewritten)
	}
	if ch.IsTailRewrite(len(cur)) {
		t.Fatalf("five changed items in a %d-item request were excused as a tail rewrite", len(cur))
	}
}

// And the count is not everything either: one changed item at the very end of a
// long request is a tail rewrite even though one item is a rounding error.
func TestOneChangedItemAtTheEndIsATailRewrite(t *testing.T) {
	var prev, cur []llm.Item
	for i := 0; i < 199; i++ {
		prev = append(prev, llm.Item{Kind: "text", Role: llm.RoleUser, Text: fmt.Sprintf("m%d", i)})
		cur = append(cur, llm.Item{Kind: "text", Role: llm.RoleUser, Text: fmt.Sprintf("m%d", i)})
	}
	prev = append(prev, llm.Item{Kind: "text", Role: llm.RoleAssistant, Text: "the old wording"})
	cur = append(cur, llm.Item{Kind: "text", Role: llm.RoleAssistant, Text: "a different wording entirely"})
	ch := classifyPrefixChange(cacheSignature(prev), cacheSignature(cur))
	if !ch.IsTailRewrite(len(cur)) {
		t.Fatalf("one changed item at the end of %d was called a regression", len(cur))
	}
}

// An append is not a rewrite at any size, and an identical shape is not either.
func TestNoDivergenceIsNeverATailRewrite(t *testing.T) {
	items := []llm.Item{
		{Kind: "text", Role: llm.RoleUser, Text: "a"},
		{Kind: "text", Role: llm.RoleAssistant, Text: "b"},
	}
	sig := cacheSignature(items)
	if ch := classifyPrefixChange(sig, sig); ch.IsTailRewrite(len(items)) {
		t.Fatalf("an unchanged request was called a tail rewrite: %q", ch.Note)
	}
	extended := cacheSignature(append(append([]llm.Item(nil), items...),
		llm.Item{Kind: "text", Role: llm.RoleUser, Text: "c"}))
	if ch := classifyPrefixChange(sig, extended); ch.IsTailRewrite(len(items) + 1) {
		t.Fatalf("an append was called a tail rewrite: %q", ch.Note)
	}
}
