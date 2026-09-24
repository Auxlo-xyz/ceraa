package evaluation

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreRecordsMetricsAndSummarizes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaluation.json")
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(RunMetrics{ID: "one", Success: true, Tokens: 10, Requests: 1, Cost: 0.25}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRun("two", RunMetrics{Success: false, Tokens: 20, Requests: 2, Cost: 0.75}); err != nil {
		t.Fatal(err)
	}

	summary, err := store.Summary()
	if err != nil {
		t.Fatal(err)
	}
	if summary.TotalRuns != 2 || summary.Successes != 1 || summary.Tokens != 30 || summary.Requests != 3 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if summary.Quality != 0.5 || summary.CostPerTask != 0.5 {
		t.Fatalf("unexpected derived summary values: %+v", summary)
	}

	loaded, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := loaded.Summary()
	if err != nil {
		t.Fatal(err)
	}
	if persisted != summary {
		t.Fatalf("expected persisted summary %+v, got %+v", summary, persisted)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".evaluation.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files remain: %v", matches)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestGuardRejectsQualityRegressionAndCostRegression(t *testing.T) {
	baseline := Summarize([]RunMetrics{
		{Success: true, Cost: 1},
		{Success: true, Cost: 1},
	})
	if err := Guard(baseline, baseline, 1); err != nil {
		t.Fatal(err)
	}
	if err := Guard(Summarize([]RunMetrics{{Success: true}, {Success: false}}), baseline, 1); !errors.Is(err, ErrQualityBelowBaseline) {
		t.Fatalf("expected quality regression, got %v", err)
	}
	if err := Guard(Summarize([]RunMetrics{{Success: true, Cost: 1}, {Success: true, Cost: 3}}), baseline, 1); !errors.Is(err, ErrCostPerTaskAboveThreshold) {
		t.Fatalf("expected cost regression, got %v", err)
	}
}

func TestStoreBaselineGuardUsesDurableBaseline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaluation.json")
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	baseline := Summarize([]RunMetrics{{Success: true, Cost: 1}, {Success: true, Cost: 1}})
	if err := store.SetBaseline("release", baseline); err != nil {
		t.Fatal(err)
	}
	if err := store.Guard("release", baseline, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.Guard("release", Summarize([]RunMetrics{{Success: true, Cost: 2}}), 1); !errors.Is(err, ErrCostPerTaskAboveThreshold) {
		t.Fatalf("expected stored baseline guard failure, got %v", err)
	}
	if err := store.Guard("missing", baseline, 1); !errors.Is(err, ErrBaselineNotFound) {
		t.Fatalf("expected missing baseline error, got %v", err)
	}
}

func TestSetBaselineFromRuns(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "evaluation.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(RunMetrics{Success: true, Cost: 0.5}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBaselineFromRuns("main"); err != nil {
		t.Fatal(err)
	}
	baseline, err := store.Baseline("main")
	if err != nil {
		t.Fatal(err)
	}
	if baseline.TotalRuns != 1 || baseline.Quality != 1 || baseline.CostPerTask != 0.5 {
		t.Fatalf("unexpected baseline: %+v", baseline)
	}
}
