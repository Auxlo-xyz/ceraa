package coordinator

import (
	"fmt"
	"strings"
	"testing"
)

// The cap had been set at 12,000 and had fired on 1 result in 766, which does not
// say what a lower cap would cost. These pin the accounting that answers it.
func TestRecordToolOutputCountsEveryResultNotJustTrimmedOnes(t *testing.T) {
	ResetToolOutputStats()
	defer ResetToolOutputStats()

	recordToolOutput(500, 500)
	recordToolOutput(9000, 5000)
	recordToolOutput(20000, 4000)

	s := ToolOutputStats()
	if s.Calls != 3 {
		t.Errorf("Calls = %d, want 3; a rate measured only over trimmed results is 100%% by construction", s.Calls)
	}
	if s.Trimmed != 2 {
		t.Errorf("Trimmed = %d, want 2", s.Trimmed)
	}
	if want := int64(500 + 9000 + 20000); s.BytesIn != want {
		t.Errorf("BytesIn = %d, want %d", s.BytesIn, want)
	}
	if want := int64(500 + 5000 + 4000); s.BytesOut != want {
		t.Errorf("BytesOut = %d, want %d", s.BytesOut, want)
	}
	if s.LargestIn != 20000 {
		t.Errorf("LargestIn = %d, want 20000", s.LargestIn)
	}
	if s.Saved() != s.BytesIn-s.BytesOut {
		t.Errorf("Saved = %d, want %d", s.Saved(), s.BytesIn-s.BytesOut)
	}
}

func TestRecordToolOutputIgnoresEmptyResults(t *testing.T) {
	ResetToolOutputStats()
	defer ResetToolOutputStats()
	recordToolOutput(0, 0)
	if s := ToolOutputStats(); s.Calls != 0 {
		t.Errorf("an empty result was counted: %d calls", s.Calls)
	}
}

// The "would have saved" figure is the number the cap decision needs, so it has to
// grow as the threshold drops and never exceed the total.
func TestBytesSavedAtGrowsAsTheThresholdDrops(t *testing.T) {
	ResetToolOutputStats()
	defer ResetToolOutputStats()
	recordToolOutput(30000, 30000)
	recordToolOutput(5000, 5000)
	s := ToolOutputStats()

	loose := s.BytesSavedAt(20000)
	tight := s.BytesSavedAt(4000)
	tighter := s.BytesSavedAt(1000)
	if !(loose < tight && tight < tighter) {
		t.Errorf("saved does not grow as the cap drops: 20k=%d 4k=%d 1k=%d", loose, tight, tighter)
	}
	if tighter > s.BytesIn {
		t.Errorf("saved at 1k (%d) exceeds all input (%d), so the estimate is wrong", tighter, s.BytesIn)
	}
	// A threshold above the largest result saves nothing.
	if got := s.BytesSavedAt(1_000_000); got != 0 {
		t.Errorf("a cap above the largest result saved %d, want 0", got)
	}
}

func TestToolOutputStatsLineLeadsWithTheDecision(t *testing.T) {
	ResetToolOutputStats()
	defer ResetToolOutputStats()
	if line := ToolOutputStatsLine(12000); !strings.Contains(line, "No results") {
		t.Errorf("with no measurements the line should say so: %q", line)
	}

	recordToolOutput(30000, 4000)
	recordToolOutput(1000, 1000)
	line := ToolOutputStatsLine(12000)
	for _, want := range []string{"cap 12.0k", "Results 2", "trimmed 1", "At 8.0k", "At 4.0k"} {
		if !strings.Contains(line, want) {
			t.Errorf("line is missing %q:\n%s", want, line)
		}
	}
}

// A cap at or below the candidate thresholds must not be suggested against itself.
func TestToolOutputStatsLineSkipsCandidatesAtOrAboveTheCap(t *testing.T) {
	ResetToolOutputStats()
	defer ResetToolOutputStats()
	recordToolOutput(9000, 9000)
	line := ToolOutputStatsLine(4000)
	if strings.Contains(line, "At 4.0k") {
		t.Errorf("suggested a cap equal to the current one:\n%s", line)
	}
	if !strings.Contains(line, "At 8.0k") && !strings.Contains(line, "cap 4.0k") {
		t.Errorf("no useful comparison offered:\n%s", line)
	}
}

func TestTrimStatsRateOnNoData(t *testing.T) {
	var s TrimStats
	if s.TrimRate() != 0 {
		t.Errorf("TrimRate on no data = %v, want 0", s.TrimRate())
	}
}

func TestHumanChars(t *testing.T) {
	cases := map[int]string{500: "500", 1500: "1.5k", 2_500_000: "2.5M"}
	for in, want := range cases {
		if got := humanChars(in); got != want {
			t.Errorf("humanChars(%d) = %q, want %q", in, got, want)
		}
	}
	_ = fmt.Sprint()
}
