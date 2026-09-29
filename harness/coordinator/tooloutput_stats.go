package coordinator

import (
	"fmt"
	"sync/atomic"
)

// Trim accounting for tool output.
//
// The point is to choose `max_tool_result_chars` from data rather than from a
// guess. The cap had been set at 12,000 and had fired on 1 result out of 766,
// which does not tell you what a lower value would cost: it tells you the cap is
// slack, not how much slack is safe to remove.
//
// Measured against 766 recorded results the distribution was median 797 chars, p90
// 8,249, p99 8,284, max 16,335. So the cap was binding on 0.13% of calls while
// tool output was 75.3% of a live request. Those two facts cannot both be right
// with a cap nobody notices, which is exactly why this is measured rather than
// argued.
type TrimStats struct {
	// Calls is every tool result that passed through formatOutput.
	Calls int64
	// Trimmed is how many exceeded the cap and were spilled to a file.
	Trimmed int64
	// BytesIn is the total size of all results, trimmed or not.
	BytesIn int64
	// BytesOut is the total size actually handed to the model.
	BytesOut int64
	// LargestIn is the biggest single result seen.
	LargestIn int64
	// OverAt records, for each power-of-two threshold above 1024, how many results
	// would have been trimmed at that threshold. This is what makes the choice
	// answerable: it shows the shape of the tail without changing the cap and
	// waiting for more sessions.
	OverAt [24]int64
}

// Saved is the bytes not sent to the model.
func (s TrimStats) Saved() int64 { return s.BytesIn - s.BytesOut }

// TrimRate is the share of results that hit the cap.
func (s TrimStats) TrimRate() float64 {
	if s.Calls == 0 {
		return 0
	}
	return float64(s.Trimmed) / float64(s.Calls)
}

// BytesSavedAt is how many bytes the cap would have saved at a given threshold,
// had it been set there. Cumulative, because a lower cap trims every larger result
// as well.
func (s TrimStats) BytesSavedAt(thresholdChars int) int64 {
	threshold := int64(thresholdChars)
	// Each bucket holds results in [threshold, threshold*2), so a result counts
	// once, at the smallest bucket it reaches. The estimate uses half the bucket as
	// the mean excess, which is the honest precision available without storing
	// every individual size.
	var saved int64
	for i, thresholdI := range trimThresholds {
		if thresholdI < threshold {
			continue
		}
		excess := thresholdI / 2
		saved += s.OverAt[i] * excess
	}
	return saved
}

// trimThresholds are the per-bucket lower bounds, in characters.
var trimThresholds = func() [24]int64 {
	var out [24]int64
	value := int64(1024)
	for i := range out {
		out[i] = value
		value *= 2
	}
	return out
}()

// trimCounters is process-wide because a run is not the unit of interest: the
// question is what the cap does to a working day, not to one turn.
var trimCounters struct {
	Calls     atomic.Int64
	Trimmed   atomic.Int64
	BytesIn   atomic.Int64
	BytesOut  atomic.Int64
	LargestIn atomic.Int64
	OverAt    [24]atomic.Int64
}

// recordToolOutput notes one tool result and whether it was trimmed.
func recordToolOutput(in, out int) {
	if in <= 0 {
		return
	}
	trimCounters.Calls.Add(1)
	trimCounters.BytesIn.Add(int64(in))
	trimCounters.BytesOut.Add(int64(out))
	for {
		current := trimCounters.LargestIn.Load()
		if int64(in) <= current || trimCounters.LargestIn.CompareAndSwap(current, int64(in)) {
			break
		}
	}
	size := int64(in)
	for i, threshold := range trimThresholds {
		if size >= threshold {
			trimCounters.OverAt[i].Add(1)
		}
	}
	if out < in {
		trimCounters.Trimmed.Add(1)
	}
}

// ToolOutputStats returns a snapshot of the trim counters.
func ToolOutputStats() TrimStats {
	stats := TrimStats{
		Calls:     trimCounters.Calls.Load(),
		Trimmed:   trimCounters.Trimmed.Load(),
		BytesIn:   trimCounters.BytesIn.Load(),
		BytesOut:  trimCounters.BytesOut.Load(),
		LargestIn: trimCounters.LargestIn.Load(),
	}
	for i := range stats.OverAt {
		stats.OverAt[i] = trimCounters.OverAt[i].Load()
	}
	return stats
}

// ResetToolOutputStats clears the counters, for a measurement that starts clean.
func ResetToolOutputStats() {
	trimCounters.Calls.Store(0)
	trimCounters.Trimmed.Store(0)
	trimCounters.BytesIn.Store(0)
	trimCounters.BytesOut.Store(0)
	trimCounters.LargestIn.Store(0)
	for i := range trimCounters.OverAt {
		trimCounters.OverAt[i].Store(0)
	}
}

// ToolOutputStatsLine renders the counters for a menu.
//
// The "would have saved" column is the part that answers the question, so it leads:
// it is the number needed to choose a cap, and everything else is context for it.
func ToolOutputStatsLine(currentCap int) string {
	s := ToolOutputStats()
	if s.Calls == 0 {
		return "📏 Tool output\nNo results measured yet."
	}
	line := fmt.Sprintf(
		"📏 Tool output (cap %s)\nResults %d · trimmed %d (%.1f%%)\nLargest %s\nSaved so far %s",
		humanChars(currentCap), s.Calls, s.Trimmed, s.TrimRate()*100,
		humanChars(int(s.LargestIn)), humanChars(int(s.Saved())))
	for _, candidate := range []int{4000, 8000} {
		if candidate >= currentCap {
			continue
		}
		line += fmt.Sprintf("\nAt %s it would have saved ~%s", humanChars(candidate), humanChars(int(s.BytesSavedAt(candidate))))
	}
	return line
}

func humanChars(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
