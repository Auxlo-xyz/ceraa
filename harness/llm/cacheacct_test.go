package llm

import (
	"ceraa/internal/logger"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCacheAcctHitRateMath(t *testing.T) {
	cases := []struct {
		name     string
		reported UsageInfo
		want     float64
	}{
		{
			name:     "three quarters cached",
			reported: UsageInfo{InputTokens: 1000, CachedInputTokens: 750},
			want:     75.0,
		},
		{
			name:     "no cached tokens is zero",
			reported: UsageInfo{InputTokens: 1000, CachedInputTokens: 0},
			want:     0,
		},
		{
			name:     "split reporting selects combined denominator",
			reported: UsageInfo{InputTokens: 250, CachedInputTokens: 750},
			want:     75.0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cacheHitPercent(tc.reported)
			if got != tc.want {
				t.Errorf("cacheHitPercent(%+v) = %.1f, want %.1f", tc.reported, got, tc.want)
			}
		})
	}
}

func TestRecordTurnZeroInputNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cacheacct.log")
	if _, err := logger.Initialize(path); err != nil {
		t.Fatalf("logger.Initialize: %v", err)
	}

	RecordTurn("api.example.com", "test-model", UsageInfo{}, 1234)
	if err := logger.Flush(); err != nil {
		t.Fatalf("logger.Flush: %v", err)
	}
	if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), "cacheacct") {
		t.Fatalf("zero input tokens should log nothing, got %q", string(data))
	}

	RecordTurn("api.example.com", "test-model",
		UsageInfo{InputTokens: 1000, CachedInputTokens: 750, OutputTokens: 50}, 1234)
	if err := logger.Flush(); err != nil {
		t.Fatalf("logger.Flush: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile: %v", err)
	}
	line := string(data)
	for _, want := range []string{
		"cacheacct", "api.example.com", "test-model",
		"input=1000", "cached=750", "output=50", "hit=75.0%", "local_estimate=1234",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("log line should contain %q, got %q", want, line)
		}
	}
	if got := strings.Count(line, "cacheacct"); got != 1 {
		t.Errorf("expected exactly one cacheacct line, got %d in %q", got, line)
	}
}
