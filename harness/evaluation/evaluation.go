package evaluation

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"sooraa/harness/flock"
)

const schemaVersion = 1

var (
	ErrQualityBelowBaseline      = errors.New("candidate quality is below baseline")
	ErrCostPerTaskAboveThreshold = errors.New("candidate cost per task is above threshold")
	ErrCostUnknown               = errors.New("candidate cost is unknown for one or more tasks")
	ErrBaselineNotFound          = errors.New("baseline not found")
)

type RunMetrics struct {
	ID         string    `json:"id,omitempty"`
	Success    bool      `json:"success"`
	Tokens     int       `json:"tokens"`
	Requests   int       `json:"requests"`
	Cost       float64   `json:"cost"`
	CostKnown  bool      `json:"cost_known"`
	RecordedAt time.Time `json:"recorded_at,omitempty"`
}

type Summary struct {
	TotalRuns     int     `json:"total_runs"`
	Successes     int     `json:"successes"`
	Tokens        int     `json:"tokens"`
	Requests      int     `json:"requests"`
	Cost          float64 `json:"cost"`
	KnownCostRuns int     `json:"known_cost_runs"`
	Quality       float64 `json:"quality"`
	SuccessRate   float64 `json:"success_rate"`
	CostPerTask   float64 `json:"cost_per_task"`
}

type state struct {
	Version   int                `json:"version"`
	Runs      []RunMetrics       `json:"runs"`
	Baselines map[string]Summary `json:"baselines,omitempty"`
}

type Store struct {
	mu       sync.Mutex
	path     string
	lockPath string
}

func New(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create evaluation directory: %w", err)
	}
	return &Store{path: path, lockPath: path + ".lock"}, nil
}

func NewStore(path string) (*Store, error) {
	return New(path)
}

func (s *Store) Record(metrics RunMetrics) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withLock(func() error {
		doc, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if metrics.Tokens < 0 || metrics.Requests < 0 || metrics.Cost < 0 || math.IsNaN(metrics.Cost) || math.IsInf(metrics.Cost, 0) {
			return errors.New("metrics must be non-negative and finite")
		}
		if metrics.RecordedAt.IsZero() {
			metrics.RecordedAt = time.Now()
		}
		doc.Runs = append(doc.Runs, metrics)
		return s.saveUnlocked(doc)
	})
}

func (s *Store) RecordRun(id string, metrics RunMetrics) error {
	metrics.ID = id
	return s.Record(metrics)
}

func (s *Store) Runs() ([]RunMetrics, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var runs []RunMetrics
	err := s.withLock(func() error {
		doc, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		runs = append([]RunMetrics(nil), doc.Runs...)
		return nil
	})
	return runs, err
}

func (s *Store) Summary() (Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var summary Summary
	err := s.withLock(func() error {
		doc, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		summary = Summarize(doc.Runs)
		return nil
	})
	return summary, err
}

func Summarize(runs []RunMetrics) Summary {
	summary := Summary{TotalRuns: len(runs)}
	for _, run := range runs {
		if run.Success {
			summary.Successes++
		}
		summary.Tokens += run.Tokens
		summary.Requests += run.Requests
		if run.CostKnown || run.Cost > 0 {
			summary.Cost += run.Cost
			summary.KnownCostRuns++
		}
	}
	summary = normalizeSummary(summary)
	return summary
}

func (s *Store) SetBaseline(name string, baseline Summary) error {
	if name == "" {
		return errors.New("baseline name is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withLock(func() error {
		doc, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if doc.Baselines == nil {
			doc.Baselines = make(map[string]Summary)
		}
		doc.Baselines[name] = normalizeSummary(baseline)
		return s.saveUnlocked(doc)
	})
}

func (s *Store) SaveBaseline(name string, baseline Summary) error {
	return s.SetBaseline(name, baseline)
}

func (s *Store) SetBaselineFromRuns(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withLock(func() error {
		doc, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if doc.Baselines == nil {
			doc.Baselines = make(map[string]Summary)
		}
		doc.Baselines[name] = Summarize(doc.Runs)
		return s.saveUnlocked(doc)
	})
}

func (s *Store) Baseline(name string) (Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var baseline Summary
	err := s.withLock(func() error {
		doc, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		value, ok := doc.Baselines[name]
		if !ok {
			return fmt.Errorf("%w: %s", ErrBaselineNotFound, name)
		}
		baseline = normalizeSummary(value)
		return nil
	})
	return baseline, err
}

func (s *Store) GetBaseline(name string) (Summary, error) {
	return s.Baseline(name)
}

func (s *Store) ListBaselines() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var names []string
	err := s.withLock(func() error {
		doc, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		names = make([]string, 0, len(doc.Baselines))
		for name := range doc.Baselines {
			names = append(names, name)
		}
		sort.Strings(names)
		return nil
	})
	return names, err
}

func (s *Store) Guard(name string, candidate Summary, maxCostPerTask float64) error {
	baseline, err := s.Baseline(name)
	if err != nil {
		return err
	}
	return Guard(candidate, baseline, maxCostPerTask)
}

func (s *Store) Check(name string, candidate Summary, maxCostPerTask float64) error {
	return s.Guard(name, candidate, maxCostPerTask)
}

func Guard(candidate, baseline Summary, maxCostPerTask float64) error {
	return GuardWithConfig(candidate, baseline, GuardConfig{MaxCostPerTask: maxCostPerTask})
}

type GuardConfig struct {
	MaxCostPerTask float64
}

func GuardWithConfig(candidate, baseline Summary, config GuardConfig) error {
	if config.MaxCostPerTask < 0 || math.IsNaN(config.MaxCostPerTask) || math.IsInf(config.MaxCostPerTask, 0) {
		return errors.New("cost per task threshold must be non-negative and finite")
	}
	candidate = normalizeSummary(candidate)
	baseline = normalizeSummary(baseline)
	if candidate.Quality < baseline.Quality {
		return fmt.Errorf("%w: candidate %.6f, baseline %.6f", ErrQualityBelowBaseline, candidate.Quality, baseline.Quality)
	}
	if baseline.KnownCostRuns > 0 && candidate.KnownCostRuns < candidate.TotalRuns {
		return fmt.Errorf("%w: %d of %d tasks have unknown cost", ErrCostUnknown, candidate.TotalRuns-candidate.KnownCostRuns, candidate.TotalRuns)
	}
	if candidate.CostPerTask > config.MaxCostPerTask {
		return fmt.Errorf("%w: candidate %.6f, threshold %.6f", ErrCostPerTaskAboveThreshold, candidate.CostPerTask, config.MaxCostPerTask)
	}
	return nil
}

func Check(candidate, baseline Summary, maxCostPerTask float64) error {
	return Guard(candidate, baseline, maxCostPerTask)
}

func normalizeSummary(summary Summary) Summary {
	if summary.TotalRuns > 0 {
		summary.Quality = float64(summary.Successes) / float64(summary.TotalRuns)
		summary.SuccessRate = summary.Quality
		if summary.KnownCostRuns > 0 {
			summary.CostPerTask = summary.Cost / float64(summary.KnownCostRuns)
		}
	}
	return summary
}

func (s *Store) loadUnlocked() (*state, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &state{Version: schemaVersion, Runs: []RunMetrics{}, Baselines: map[string]Summary{}}, nil
		}
		return nil, fmt.Errorf("read evaluation state: %w", err)
	}
	var doc state
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("decode evaluation state: %w", err)
	}
	if doc.Version == 0 {
		doc.Version = schemaVersion
	}
	if doc.Runs == nil {
		doc.Runs = []RunMetrics{}
	}
	if doc.Baselines == nil {
		doc.Baselines = map[string]Summary{}
	}
	return &doc, nil
}

func (s *Store) saveUnlocked(doc *state) error {
	doc.Version = schemaVersion
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode evaluation state: %w", err)
	}
	return atomicWrite(s.path, data)
}

func (s *Store) withLock(fn func() error) error {
	lock, err := flock.Acquire(s.lockPath)
	if err != nil {
		return fmt.Errorf("lock evaluation state: %w", err)
	}
	defer lock.Release()
	return fn()
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create evaluation temp file: %w", err)
	}
	tmpPath := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod evaluation temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write evaluation temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync evaluation temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close evaluation temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace evaluation state: %w", err)
	}
	removeTemp = false
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}
