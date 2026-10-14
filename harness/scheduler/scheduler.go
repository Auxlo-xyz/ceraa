package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ceraa/harness/flock"
)

const schemaVersion = 1

var (
	ErrJobNotFound     = errors.New("job not found")
	ErrJobExists       = errors.New("job already exists")
	ErrInvalidSchedule = errors.New("invalid schedule")
	ErrScheduleInPast  = errors.New("schedule has no future run")
	ErrNoRunner        = errors.New("runner callback is nil")
	ErrAttemptConflict = errors.New("scheduler attempt conflicts with current execution")
)

type Status string

const (
	StatusIdle    Status = "idle"
	StatusRunning Status = "running"
	StatusSuccess Status = "success"
	StatusError   Status = "error"
)

type Job struct {
	ID         string        `json:"id"`
	Name       string        `json:"name,omitempty"`
	Prompt     string        `json:"prompt,omitempty"`
	Command    string        `json:"command,omitempty"`
	Args       []string      `json:"args,omitempty"`
	Data       any           `json:"data,omitempty"`
	SessionID  string        `json:"session_id,omitempty"`
	ChatID     int64         `json:"chat_id,omitempty"`
	Project    string        `json:"project,omitempty"`
	Interval   time.Duration `json:"interval,omitempty"`
	RunAt      time.Time     `json:"run_at,omitempty"`
	Cron       string        `json:"cron,omitempty"`
	Timezone   string        `json:"timezone,omitempty"`
	NextRunAt  time.Time     `json:"next_run_at,omitempty"`
	LastRunAt  time.Time     `json:"last_run_at,omitempty"`
	LastStatus Status        `json:"last_status"`
	LastError  string        `json:"last_error,omitempty"`
	RunCount   int           `json:"run_count"`
	CreatedAt  time.Time     `json:"created_at"`
	UpdatedAt  time.Time     `json:"updated_at"`
	AttemptID  string        `json:"attempt_id,omitempty"`
}

type Callback func(context.Context, Job) error

type state struct {
	Version int   `json:"version"`
	Jobs    []Job `json:"jobs"`
}

type Store struct {
	mu       sync.Mutex
	path     string
	lockPath string
	Now      func() time.Time
	running  map[string]struct{}
}

func New(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create scheduler directory: %w", err)
	}
	return &Store{path: path, lockPath: path + ".lock", Now: time.Now, running: make(map[string]struct{})}, nil
}

func NewStore(path string) (*Store, error) {
	return New(path)
}

func (s *Store) Add(job Job) error {
	if job.ID == "" {
		return fmt.Errorf("%w: job id is empty", ErrInvalidSchedule)
	}
	now := s.currentTime()
	if err := validateSchedule(job); err != nil {
		return err
	}
	if job.NextRunAt.IsZero() {
		next, err := NextRun(job, now)
		if err != nil {
			return err
		}
		if next.IsZero() {
			return ErrScheduleInPast
		}
		job.NextRunAt = next
	}
	if job.LastStatus == "" {
		job.LastStatus = StatusIdle
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	job.UpdatedAt = now

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withLock(func() error {
		doc, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		for _, existing := range doc.Jobs {
			if existing.ID == job.ID {
				return fmt.Errorf("%w: %s", ErrJobExists, job.ID)
			}
		}
		doc.Jobs = append(doc.Jobs, cloneJob(job))
		return s.saveUnlocked(doc)
	})
}

func (s *Store) List() ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var jobs []Job
	err := s.withLock(func() error {
		doc, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		jobs = sortedJobs(doc.Jobs)
		return nil
	})
	return jobs, err
}

func (s *Store) Get(id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var job Job
	err := s.withLock(func() error {
		doc, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		for _, candidate := range doc.Jobs {
			if candidate.ID == id {
				job = cloneJob(candidate)
				return nil
			}
		}
		return fmt.Errorf("%w: %s", ErrJobNotFound, id)
	})
	return job, err
}

func (s *Store) Remove(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := false
	err := s.withLock(func() error {
		doc, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		jobs := make([]Job, 0, len(doc.Jobs))
		for _, job := range doc.Jobs {
			if job.ID == id {
				removed = true
				continue
			}
			jobs = append(jobs, job)
		}
		if !removed {
			return nil
		}
		doc.Jobs = jobs
		return s.saveUnlocked(doc)
	})
	return removed, err
}

func (s *Store) Update(id string, update func(*Job) error) error {
	if update == nil {
		return errors.New("job update is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withLock(func() error {
		doc, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		for i := range doc.Jobs {
			if doc.Jobs[i].ID != id {
				continue
			}
			copyJob := cloneJob(doc.Jobs[i])
			if err := update(&copyJob); err != nil {
				return err
			}
			if err := validateSchedule(copyJob); err != nil {
				return err
			}
			copyJob.UpdatedAt = s.currentTime()
			doc.Jobs[i] = cloneJob(copyJob)
			return s.saveUnlocked(doc)
		}
		return fmt.Errorf("%w: %s", ErrJobNotFound, id)
	})
}

func (s *Store) ClaimDue(now time.Time) ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var claimed []Job
	err := s.withLock(func() error {
		doc, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		changed := false
		for i := range doc.Jobs {
			job := &doc.Jobs[i]
			if !IsDue(*job, now) || job.LastStatus == StatusRunning {
				continue
			}
			next, err := s.nextRun(*job, now)
			if err != nil {
				return err
			}
			job.LastStatus = StatusRunning
			job.LastError = ""
			job.LastRunAt = now
			job.RunCount++
			job.AttemptID = fmt.Sprintf("%s:%d", job.ID, job.RunCount)
			job.NextRunAt = next
			job.UpdatedAt = now
			claimed = append(claimed, cloneJob(*job))
			changed = true
		}
		if !changed {
			return nil
		}
		return s.saveUnlocked(doc)
	})
	return claimed, err
}

func (s *Store) ClaimDueJobs(now time.Time) ([]Job, error) {
	return s.ClaimDue(now)
}

func (s *Store) Claim(id string, now time.Time) (Job, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var claimed Job
	found := false
	err := s.withLock(func() error {
		doc, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		for i := range doc.Jobs {
			job := &doc.Jobs[i]
			if job.ID != id {
				continue
			}
			found = true
			if !IsDue(*job, now) || job.LastStatus == StatusRunning {
				return nil
			}
			next, err := s.nextRun(*job, now)
			if err != nil {
				return err
			}
			job.LastStatus = StatusRunning
			job.LastError = ""
			job.LastRunAt = now
			job.RunCount++
			job.AttemptID = fmt.Sprintf("%s:%d", job.ID, job.RunCount)
			job.NextRunAt = next
			job.UpdatedAt = now
			claimed = cloneJob(*job)
			return s.saveUnlocked(doc)
		}
		return fmt.Errorf("%w: %s", ErrJobNotFound, id)
	})
	return claimed, found && claimed.ID != "", err
}

// Complete records a job result for callers that do not use attempt fencing.
// New execution paths should use CompleteAttempt.
func (s *Store) Complete(id string, status Status, runError string) error {
	return s.complete(id, "", status, runError)
}

// CompleteAttempt records a result only if attemptID is still the current
// claimed execution. A late callback from an older attempt cannot overwrite a
// newer claim after recovery or retry.
func (s *Store) CompleteAttempt(id, attemptID string, status Status, runError string) error {
	return s.complete(id, attemptID, status, runError)
}

func (s *Store) complete(id, attemptID string, status Status, runError string) error {
	if status != StatusSuccess && status != StatusError {
		return fmt.Errorf("%w: completion status %q", ErrInvalidSchedule, status)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withLock(func() error {
		doc, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		for i := range doc.Jobs {
			if doc.Jobs[i].ID != id {
				continue
			}
			if attemptID != "" && doc.Jobs[i].AttemptID != attemptID {
				return fmt.Errorf("%w: job %q attempt %q", ErrAttemptConflict, id, attemptID)
			}
			doc.Jobs[i].LastStatus = status
			doc.Jobs[i].LastError = runError
			doc.Jobs[i].UpdatedAt = s.currentTime()
			return s.saveUnlocked(doc)
		}
		return fmt.Errorf("%w: %s", ErrJobNotFound, id)
	})
}

func (s *Store) Finish(id string, runError error) error {
	if runError == nil {
		return s.Complete(id, StatusSuccess, "")
	}
	return s.Complete(id, StatusError, runError.Error())
}

// FinishAttempt is the fenced form of Finish.
func (s *Store) FinishAttempt(id, attemptID string, runError error) error {
	if runError == nil {
		return s.CompleteAttempt(id, attemptID, StatusSuccess, "")
	}
	return s.CompleteAttempt(id, attemptID, StatusError, runError.Error())
}

func (s *Store) MarkSuccess(id string) error {
	return s.Finish(id, nil)
}

func (s *Store) MarkError(id string, runError error) error {
	return s.Finish(id, runError)
}

func (s *Store) Recover(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withLock(func() error {
		doc, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		changed := false
		for i := range doc.Jobs {
			job := &doc.Jobs[i]
			if job.LastStatus != StatusRunning {
				continue
			}
			job.LastStatus = StatusError
			job.LastError = "interrupted by restart"
			job.UpdatedAt = now
			changed = true
		}
		if !changed {
			return nil
		}
		return s.saveUnlocked(doc)
	})
}

func (s *Store) Run(ctx context.Context, callback Callback) error {
	return s.RunWithInterval(ctx, time.Second, callback)
}

func (s *Store) RunLoop(ctx context.Context, interval time.Duration, callback Callback) error {
	return s.RunWithInterval(ctx, interval, callback)
}

func (s *Store) RunWithInterval(ctx context.Context, interval time.Duration, callback Callback) error {
	if callback == nil {
		return ErrNoRunner
	}
	if interval <= 0 {
		return errors.New("runner interval must be positive")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var jobs sync.WaitGroup
	defer jobs.Wait()
	for {
		claimed, err := s.ClaimDue(s.currentTime())
		if err != nil {
			return err
		}
		for _, job := range claimed {
			s.mu.Lock()
			if _, exists := s.running[job.ID]; exists {
				s.mu.Unlock()
				continue
			}
			s.running[job.ID] = struct{}{}
			s.mu.Unlock()
			jobs.Add(1)
			go func(job Job) {
				defer jobs.Done()
				defer func() {
					s.mu.Lock()
					delete(s.running, job.ID)
					s.mu.Unlock()
				}()
				runError := callback(ctx, job)
				_ = s.FinishAttempt(job.ID, job.AttemptID, runError)
			}(job)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Store) RunJob(ctx context.Context, job Job, callback Callback) error {
	if callback == nil {
		return ErrNoRunner
	}
	return s.Finish(job.ID, callback(ctx, job))
}

func NextRun(job Job, from time.Time) (time.Time, error) {
	if !job.NextRunAt.IsZero() && job.NextRunAt.After(from) {
		return job.NextRunAt, nil
	}
	if job.Cron != "" {
		return nextCronRun(job.Cron, job.Timezone, from)
	}
	if !job.RunAt.IsZero() {
		if job.RunAt.After(from) {
			return job.RunAt, nil
		}
		return time.Time{}, nil
	}
	if job.Interval <= 0 {
		return time.Time{}, ErrInvalidSchedule
	}
	base := from
	if !job.NextRunAt.IsZero() && !job.NextRunAt.After(from) {
		base = job.NextRunAt
	}
	base = base.Add(job.Interval)
	for !base.After(from) {
		base = base.Add(job.Interval)
	}
	if !base.After(from) {
		return time.Time{}, ErrInvalidSchedule
	}
	return base, nil
}

func (s *Store) NextRun(job Job, from time.Time) (time.Time, error) {
	return NextRun(job, from)
}

func IsDue(job Job, now time.Time) bool {
	return !job.NextRunAt.IsZero() && !job.NextRunAt.After(now)
}

func (s *Store) nextRun(job Job, from time.Time) (time.Time, error) {
	return NextRun(job, from)
}

func (s *Store) currentTime() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

func validateSchedule(job Job) error {
	count := 0
	if job.Interval > 0 {
		count++
	}
	if !job.RunAt.IsZero() {
		count++
	}
	if job.Cron != "" {
		count++
	}
	if count != 1 {
		return fmt.Errorf("%w: set exactly one interval, run_at, or cron", ErrInvalidSchedule)
	}
	if job.Interval < 0 || math.IsNaN(float64(job.Interval)) || math.IsInf(float64(job.Interval), 0) {
		return fmt.Errorf("%w: invalid interval", ErrInvalidSchedule)
	}
	if job.Cron != "" {
		if _, err := parseCron(job.Cron); err != nil {
			return err
		}
		if job.Timezone != "" {
			if _, err := time.LoadLocation(job.Timezone); err != nil {
				return fmt.Errorf("%w: invalid timezone: %v", ErrInvalidSchedule, err)
			}
		}
	}
	return nil
}

func (s *Store) loadUnlocked() (*state, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &state{Version: schemaVersion, Jobs: []Job{}}, nil
		}
		return nil, fmt.Errorf("read scheduler state: %w", err)
	}
	var doc state
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("decode scheduler state: %w", err)
	}
	if doc.Version == 0 {
		doc.Version = schemaVersion
	}
	if doc.Jobs == nil {
		doc.Jobs = []Job{}
	}
	for i := range doc.Jobs {
		if doc.Jobs[i].LastStatus == "" {
			doc.Jobs[i].LastStatus = StatusIdle
		}
	}
	return &doc, nil
}

func (s *Store) saveUnlocked(doc *state) error {
	doc.Version = schemaVersion
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode scheduler state: %w", err)
	}
	return atomicWrite(s.path, data)
}

func (s *Store) withLock(fn func() error) error {
	lock, err := flock.Acquire(s.lockPath)
	if err != nil {
		return fmt.Errorf("lock scheduler state: %w", err)
	}
	defer lock.Release()
	return fn()
}

func cloneJob(job Job) Job {
	job.Args = append([]string(nil), job.Args...)
	return job
}

func sortedJobs(jobs []Job) []Job {
	result := append([]Job(nil), jobs...)
	sort.SliceStable(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})
	return result
}

type cronFields struct {
	minute     map[int]bool
	hour       map[int]bool
	dayOfMonth map[int]bool
	month      map[int]bool
	dayOfWeek  map[int]bool
	domWild    bool
	dowWild    bool
}

var monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
var weekdayNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

func parseCron(expression string) (cronFields, error) {
	parts := strings.Fields(expression)
	if len(parts) != 5 {
		return cronFields{}, fmt.Errorf("%w: cron requires five fields", ErrInvalidSchedule)
	}
	minute, err := parseCronField(parts[0], 0, 59, nil)
	if err != nil {
		return cronFields{}, err
	}
	hour, err := parseCronField(parts[1], 0, 23, nil)
	if err != nil {
		return cronFields{}, err
	}
	dayOfMonth, domWild, err := parseCronFieldWithWild(parts[2], 1, 31, nil)
	if err != nil {
		return cronFields{}, err
	}
	month, err := parseCronField(parts[3], 1, 12, monthNames)
	if err != nil {
		return cronFields{}, err
	}
	dayOfWeek, dowWild, err := parseCronFieldWithWild(parts[4], 0, 7, weekdayNames)
	if err != nil {
		return cronFields{}, err
	}
	for weekday := range dayOfWeek {
		if weekday == 7 {
			delete(dayOfWeek, weekday)
			dayOfWeek[0] = true
		}
	}
	return cronFields{
		minute:     minute,
		hour:       hour,
		dayOfMonth: dayOfMonth,
		month:      month,
		dayOfWeek:  dayOfWeek,
		domWild:    domWild,
		dowWild:    dowWild,
	}, nil
}

func parseCronField(field string, min, max int, names map[string]int) (map[int]bool, error) {
	values, _, err := parseCronFieldWithWild(field, min, max, names)
	return values, err
}

func parseCronFieldWithWild(field string, min, max int, names map[string]int) (map[int]bool, bool, error) {
	values := make(map[int]bool)
	wildcard := field == "*" || field == "?"
	for _, token := range strings.Split(strings.ToLower(field), ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			return nil, false, fmt.Errorf("%w: empty cron field", ErrInvalidSchedule)
		}
		parts := strings.Split(token, "/")
		if len(parts) > 2 {
			return nil, false, fmt.Errorf("%w: invalid cron field %q", ErrInvalidSchedule, field)
		}
		base := parts[0]
		if base == "" {
			return nil, false, fmt.Errorf("%w: invalid cron field %q", ErrInvalidSchedule, field)
		}
		step := 1
		if len(parts) == 2 {
			value, err := strconv.Atoi(parts[1])
			if err != nil || value <= 0 {
				return nil, false, fmt.Errorf("%w: invalid cron step %q", ErrInvalidSchedule, parts[1])
			}
			step = value
		}
		var start, end int
		switch {
		case base == "*" || base == "?":
			start, end = min, max
		case strings.Contains(base, "-"):
			bounds := strings.Split(base, "-")
			if len(bounds) != 2 {
				return nil, false, fmt.Errorf("%w: invalid cron range %q", ErrInvalidSchedule, base)
			}
			var err error
			start, err = cronValue(bounds[0], min, max, names)
			if err != nil {
				return nil, false, err
			}
			end, err = cronValue(bounds[1], min, max, names)
			if err != nil {
				return nil, false, err
			}
			if start > end {
				return nil, false, fmt.Errorf("%w: invalid cron range %q", ErrInvalidSchedule, base)
			}
		default:
			value, err := cronValue(base, min, max, names)
			if err != nil {
				return nil, false, err
			}
			start, end = value, value
		}
		for value := start; value <= end; value += step {
			values[value] = true
		}
	}
	return values, wildcard, nil
}

func cronValue(raw string, min, max int, names map[string]int) (int, error) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if names != nil {
		if value, ok := names[raw]; ok {
			return value, nil
		}
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < min || value > max {
		return 0, fmt.Errorf("%w: invalid cron value %q", ErrInvalidSchedule, raw)
	}
	return value, nil
}

func nextCronRun(expression, timezone string, from time.Time) (time.Time, error) {
	fields, err := parseCron(expression)
	if err != nil {
		return time.Time{}, err
	}
	location := time.UTC
	if timezone != "" {
		location, err = time.LoadLocation(timezone)
		if err != nil {
			return time.Time{}, fmt.Errorf("%w: invalid timezone: %v", ErrInvalidSchedule, err)
		}
	}
	local := from.In(location)
	start := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), 0, 0, location).Add(time.Minute)
	for i := 0; i < 366*24*60*2; i++ {
		candidate := start.Add(time.Duration(i) * time.Minute)
		if cronMatches(fields, candidate) {
			return candidate.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("%w: no next run for cron %q", ErrInvalidSchedule, expression)
}

func cronMatches(fields cronFields, candidate time.Time) bool {
	if !fields.minute[candidate.Minute()] || !fields.hour[candidate.Hour()] || !fields.month[int(candidate.Month())] {
		return false
	}
	dom := fields.dayOfMonth[candidate.Day()]
	dow := fields.dayOfWeek[int(candidate.Weekday())]
	dayMatch := dom
	if fields.domWild && fields.dowWild {
		dayMatch = true
	} else if fields.domWild {
		dayMatch = dow
	} else if fields.dowWild {
		dayMatch = dom
	} else {
		dayMatch = dom || dow
	}
	return dayMatch
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create scheduler temp file: %w", err)
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
		return fmt.Errorf("chmod scheduler temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write scheduler temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync scheduler temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close scheduler temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace scheduler state: %w", err)
	}
	removeTemp = false
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}
