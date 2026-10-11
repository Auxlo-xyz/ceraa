package llm

import (
	"fmt"
	"strings"
	"time"
)

// Explicit prompt-cache breakpoints.
//
// Implicit caching, which is what this harness used until now, asks the provider to
// find the longest common prefix on its own. That leaves the cut to the
// provider's own heuristics, which align to minimum block sizes, so a prefix can
// be missed even though nearly all of it was byte-identical to the previous
// request. Measured here: consecutive requests were 99.85-99.97% unchanged and
// still read 0% cache.
//
// Explicit mode marks where the cache should end. Three things have to be right,
// and getting any of them wrong produces the same silence as not trying at all:
//
//  1. The marker goes on a content block, not on the request envelope. A
//     top-level cache_control is not a field the API reads; it is ignored without
//     complaint, which is what the previous implementation did.
//  2. The marked block must clear the provider's minimum cacheable size. Ours is
//     about 1,390 tokens of system prompt, above the 1,024 floor several providers
//     enforce, so the system block does clear it and is marked. It was previously
//     documented here as roughly 860 tokens and below the floor, which was once
//     true and stopped being true as prompt/system.txt grew; the comment then
//     described a breakpoint being discarded when the code was sending one. A
//     second, vaguer claim about the same figure lived in IMPLEMENTATION.md.
//     Neither was load-bearing at runtime, but both would have had a reader
//     concluding the prompt head is uncached when it is the part that is.
//
//     Either way the conversation is where the tokens are: a long session carries
//     tens of thousands against a prompt head of about thirteen hundred, so the
//     breakpoint that earns its place is the one on the last stable message.
//  3. Blocks must concatenate to the same bytes every turn. Joining blocks
//     naively means anything inserted later changes the bytes before it, so every
//     block except the last is newline-terminated: each breakpoint then covers a
//     self-terminating range that nothing downstream can disturb.
//
// TTL is opt-in and validated rather than defaulted. A 1-hour write costs 2x base
// input against 1.25x for the 5-minute default, so on a provider that never
// serves a read it is a straight loss, and roughly 40% of measured runs report a
// flat zero. Defaulting it would tax exactly the runs that are already worst.

// CacheTTL is a validated prompt-cache lifetime.
type CacheTTL string

const (
	// CacheTTL5m is the provider default when no TTL is set.
	CacheTTL5m CacheTTL = "5m"
	// CacheTTL1h costs 2x base input on write against 1.25x for 5m.
	CacheTTL1h CacheTTL = "1h"
)

// ParseCacheTTL validates a configured TTL.
//
// Only the two values providers actually document are accepted. An unrecognised
// value is an error rather than a silent passthrough, because a typo forwarded to
// the API is either rejected outright or, worse, accepted as something other than
// what was intended.
func ParseCacheTTL(raw string) (CacheTTL, error) {
	switch CacheTTL(strings.ToLower(strings.TrimSpace(raw))) {
	case "":
		return CacheTTL5m, nil
	case CacheTTL5m:
		return CacheTTL5m, nil
	case CacheTTL1h:
		return CacheTTL1h, nil
	default:
		return "", fmt.Errorf("unsupported prompt cache ttl %q: use %q or %q", raw, CacheTTL5m, CacheTTL1h)
	}
}

// Duration returns the wall-clock lifetime a TTL represents.
func (t CacheTTL) Duration() time.Duration {
	if t == CacheTTL1h {
		return time.Hour
	}
	return 5 * time.Minute
}

// PromptCacheTTLFor is how long a cached prefix is expected to survive for a
// provider. It is what the keepalive refreshes against, so it has to be the
// same number the breakpoints were written with rather than a second opinion.
//
// The TTL is provider-shaped rather than purely configured: a Messages endpoint
// whose profile named none gets an hour (see cacheTTLFor for why), and every
// other provider gets the five-minute default. A profile that named one always
// wins.
func PromptCacheTTLFor(cfg ProviderConfig) time.Duration {
	return cacheTTLFor(cfg).Duration()
}

// MinCacheableTokens is the smallest block several providers will cache. A
// breakpoint below it is discarded, so the system prompt on its own is not a
// useful breakpoint here and the conversation carries the real weight.
const MinCacheableTokens = 1024

// MaxCacheBreakpoints is the documented ceiling. Going past it is a hard error
// from the API, so extra markers are dropped rather than risking the request.
const MaxCacheBreakpoints = 4

// cacheControl builds the marker for a block.
func cacheControl(ttl CacheTTL) map[string]any {
	control := map[string]any{"type": "ephemeral"}
	if ttl == CacheTTL1h {
		control["ttl"] = string(CacheTTL1h)
	}
	return control
}

// SystemBlocks splits a system prompt into cacheable and volatile parts and
// returns them as content blocks.
//
// The split point is supplied by the caller because only it knows which regions
// are stable. Everything before the seam is marked; everything from the seam on is
// left unmarked, so a date rollover or a memory rewrite re-reads only the tail.
func SystemBlocks(system string, seam int, ttl CacheTTL) []map[string]any {
	if system == "" {
		return nil
	}
	seam = clamp(seam, 0, len(system))

	// The seam lands after the separator that follows the stable text, so the
	// stable block already ends in a newline and the volatile block carries none
	// extra. Trimming here and re-adding a newline on join produced a prompt one
	// byte longer than the un-split original, which changes the cached prefix on
	// every request and silently defeats the cache it was meant to enable.
	stable := system[:seam]
	volatile := system[seam:]

	var blocks []map[string]any
	// A breakpoint under the provider minimum is discarded, so it is not worth
	// sending and would only imply a cache that never happens.
	if stable != "" && estimateTokens(stable) >= MinCacheableTokens {
		blocks = append(blocks, map[string]any{
			"type":          "text",
			"text":          stable,
			"cache_control": cacheControl(ttl),
		})
	}
	if volatile != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": volatile})
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": system})
	}
	return blocks
}

// FlattenBlocks rejoins blocks into the exact text a single-string system message
// would have carried.
//
// Plain concatenation, with no separator inserted. SystemBlocks places the seam
// after the existing separator, so every block but the last is already
// newline-terminated and the bytes on the wire stay identical to the un-split
// form. That identity is what makes the split invisible to a provider that
// ignores cache_control, and it is the reason the seam is not simply the length
// of the stable text.
func FlattenBlocks(blocks []map[string]any) string {
	var b strings.Builder
	for _, block := range blocks {
		text, _ := block["text"].(string)
		b.WriteString(text)
	}
	return b.String()
}

// markLastStableMessage puts a breakpoint on the final message of the history.
//
// This is the breakpoint that matters. The system prompt is a few thousand
// tokens, but a long session carries tens of thousands in the conversation, and
// that is the prefix worth not paying for twice. Marking the last message caches
// everything up to and including it.
//
// Two things were wrong here and only one of them is fixed.
//
// The `limit` counter has always been effectively one-per-call: it is
// decremented and then the function returns, because one breakpoint on the newest
// usable message already covers every earlier position. It is kept honest rather
// than removed, so the loop reads as what it does.
//
// The role check was NOT added, and that is deliberate. Skipping tool messages
// looks tidier - "a tool result cannot usefully end a cached prefix" - but
// tool output is roughly 92% of the input tokens in an agent loop, so walking
// back past them to the last user or assistant turn would discard almost the
// entire cacheable span on exactly the tool-heavy requests that need it most.
// Anthropic's documented multi-turn pattern is precisely a breakpoint on the
// last tool_result. The comment claiming otherwise was wrong.
func markLastStableMessage(msgs []map[string]any, ttl CacheTTL) {
	limit := MaxCacheBreakpoints
	for i := len(msgs) - 1; i >= 0 && limit > 0; i-- {
		msg := msgs[i]
		if !hasContent(msg) {
			continue
		}
		applyCacheControl(msg, cacheControl(ttl))
		limit--
		// One breakpoint on the newest usable message is the one that matters:
		// the provider caches everything up to a breakpoint, so the most recent
		// breakpoint already covers every earlier one. Marking older messages
		// would spend the remaining slots re-caching spans already covered.
		return
	}
}

func hasContent(msg map[string]any) bool {
	switch content := msg["content"].(type) {
	case string:
		return strings.TrimSpace(content) != ""
	case []any:
		return len(content) > 0
	default:
		return false
	}
}

// applyCacheControl sets the marker on a message, converting a string body into a
// single block if needed, because the field is only read on content blocks.
func applyCacheControl(msg map[string]any, control map[string]any) {
	switch content := msg["content"].(type) {
	case string:
		msg["content"] = []any{map[string]any{"type": "text", "text": content, "cache_control": control}}
	case []any:
		if len(content) == 0 {
			return
		}
		// Mark the final block, which is the one that closes the prefix.
		if last, ok := content[len(content)-1].(map[string]any); ok {
			last["cache_control"] = control
		} else {
			content = append(content, map[string]any{"type": "text", "text": "", "cache_control": control})
			msg["content"] = content
		}
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
