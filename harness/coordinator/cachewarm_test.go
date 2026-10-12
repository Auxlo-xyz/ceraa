package coordinator

import (
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
	note, regressed := classifyPrefixChange(base, extended)
	if regressed {
		t.Fatalf("an append reported %q as a regression", note)
	}
	if !strings.Contains(note, "appended") {
		t.Fatalf("note = %q, want it to name the append", note)
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
	if note, regressed := classifyPrefixChange(long, short); regressed {
		t.Fatalf("truncating to an identical prefix reported %q", note)
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
