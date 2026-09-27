package llm

import (
	"strings"
	"testing"
)

// The defect this replaces: the marker was written onto the request body, a field
// no API reads, so explicit mode returned 200 and cached nothing.
// bigSystem returns a system prompt over the provider's minimum cacheable size,
// with a seam splitting stable text from volatile text.
func bigSystem() (string, int) {
	stable := strings.Repeat("stable token ", 800) // ~2600 tokens
	volatile := "<current-date>\nToday is 2026-09-27.\n</current-date>"
	return stable + "\n\n" + volatile, len(stable) + 2
}

func TestExplicitModeMarksContentBlocksNotTheEnvelope(t *testing.T) {
	body := map[string]any{}
	system, seam := bigSystem()
	msgs := []map[string]any{
		{"role": "system", "content": system},
		{"role": "user", "content": "hello"},
	}
	body["messages"] = msgs
	applyCacheBreakpoints(msgs, ProviderConfig{PromptCacheMode: "explicit"}, seam)

	if _, present := body["cache_control"]; present {
		t.Error("cache_control was written to the request envelope, where nothing reads it")
	}
	blocks, ok := msgs[0]["content"].([]any)
	if !ok || len(blocks) == 0 {
		t.Fatalf("system content was not converted to blocks: %#v", msgs[0]["content"])
	}
	if _, marked := blocks[0].(map[string]any)["cache_control"]; !marked {
		t.Error("no breakpoint on the system block")
	}
}

// Providers discard a breakpoint below their minimum cacheable size. The system
// prompt here is about 860 tokens, under the 1,024 floor, so marking it alone
// would be silently dropped and read as "explicit mode does not help".
func TestASubMinimumBlockIsNotMarked(t *testing.T) {
	small := strings.Repeat("word ", 100) // well under the minimum
	blocks := SystemBlocks(small, len(small), CacheTTL5m)
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	if _, marked := blocks[0]["cache_control"]; marked {
		t.Error("a block under the provider minimum was marked and will be discarded")
	}
	if blocks[0]["text"] != small {
		t.Error("the text was altered when the breakpoint was skipped")
	}
}

// The conversation is where the tokens are, so the breakpoint that matters is on
// the last message of the history, not on the system prompt.
func TestTheLastStableMessageIsMarked(t *testing.T) {
	msgs := []map[string]any{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": "first"},
		{"role": "assistant", "content": "second"},
		{"role": "user", "content": "third"},
	}
	markLastStableMessage(msgs, CacheTTL5m)
	last := msgs[3]["content"].([]any)[0].(map[string]any)
	if _, marked := last["cache_control"]; !marked {
		t.Error("the final message carries no breakpoint, so the conversation is never cached")
	}
	if _, isString := msgs[1]["content"].(string); !isString {
		t.Error("an earlier message was marked; the prefix must close at the last one")
	}
}

// Auto mode is the default for every provider, and block arrays change the request
// shape, so auto must leave the body byte-identical to what it always sent.
func TestAutoModeLeavesTheBodyUntouched(t *testing.T) {
	msgs := []map[string]any{{"role": "system", "content": "plain string"}}
	applyCacheBreakpoints(msgs, ProviderConfig{PromptCacheMode: "auto"}, 4)
	if _, ok := msgs[0]["content"].(string); !ok {
		t.Errorf("auto mode reshaped the system content: %#v", msgs[0]["content"])
	}
}

// The newline rule is what makes a split invisible: blocks must reconcatenate to
// the same bytes, or a provider that ignores the marker sees a different prompt
// and every other comparison in this file is meaningless.
func TestFlattenBlocksReproducesTheOriginalText(t *testing.T) {
	original, seam := bigSystem()
	blocks := SystemBlocks(original, seam, CacheTTL5m)
	if len(blocks) < 2 {
		t.Fatalf("expected a split, got %d block(s)", len(blocks))
	}
	if got := FlattenBlocks(blocks); got != original {
		t.Errorf("flattened text changed:\n got %q\nwant %q", got, original)
	}
}

// Every block but the last is newline-terminated, so each breakpoint covers a
// self-terminating byte range that nothing appended later can disturb.
func TestEveryBlockButTheLastIsNewlineTerminated(t *testing.T) {
	original, seam := bigSystem()
	blocks := SystemBlocks(original, seam, CacheTTL5m)
	for i := 0; i < len(blocks)-1; i++ {
		text, _ := blocks[i]["text"].(string)
		if !strings.HasSuffix(text, "\n") {
			t.Errorf("block %d is not newline-terminated: %q", i, text)
		}
	}
}

// A typo must not be forwarded: an unrecognised TTL is either rejected by the API
// or, worse, accepted as something other than what was intended.
func TestParseCacheTTLRejectsUnknownValues(t *testing.T) {
	if _, err := ParseCacheTTL("1 hour"); err == nil {
		t.Error("an unparseable TTL was accepted")
	}
	if _, err := ParseCacheTTL("24h"); err == nil {
		t.Error("an unsupported TTL was accepted")
	}
	if got, err := ParseCacheTTL("1H"); err != nil || got != CacheTTL1h {
		t.Errorf("ParseCacheTTL(1H) = %q, %v; want 1h, nil", got, err)
	}
	if got, err := ParseCacheTTL(""); err != nil || got != CacheTTL5m {
		t.Errorf("an empty TTL should mean the 5m default, got %q, %v", got, err)
	}
}

// A 1-hour write costs 2x base input against 1.25x for 5m, so it is only sent when
// explicitly asked for.
func TestOneHourTTLCarriesItsPremium(t *testing.T) {
	system, seam := bigSystem()
	five := SystemBlocks(system, seam, CacheTTL5m)
	one := SystemBlocks(system, seam, CacheTTL1h)
	control := five[0]["cache_control"].(map[string]any)
	if _, present := control["ttl"]; present {
		t.Error("the 5m default should not carry an explicit ttl")
	}
	if got := one[0]["cache_control"].(map[string]any)["ttl"]; got != "1h" {
		t.Errorf("ttl = %v, want 1h", got)
	}
}
