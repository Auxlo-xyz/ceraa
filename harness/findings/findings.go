// Package findings is the record of what a run found, what it ruled out and
// what it left open.
//
// The gap it closes: a long recon run finds things, but nothing recorded what
// was tried, what was ruled out, or what is still unproven. Without that
// record a finding cannot be told apart from a guess, and a report that hides
// the ruled-out items and the open gaps is worse than no report, because the
// reader cannot see what was checked.
//
// The closure vocabulary is three states and no more:
//
//	confirmed      the finding is proved
//	ruled_out      it is proved not to be a problem, with the reason and the
//	               control that stops it
//	open_proof_gap it is suspected, with the step that would close it
//
// The record is deliberately dumb: one file per finding, no index, no
// database. It is written by a model that may run for hours and read by
// someone deciding whether to disclose.
package findings

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Status is the closure state of a finding.
type Status string

const (
	// StatusConfirmed means the finding is proved and reportable.
	StatusConfirmed Status = "confirmed"
	// StatusRuledOut means it is proved not to be a problem. The reason and
	// the control that stops it belong in the finding, not here.
	StatusRuledOut Status = "ruled_out"
	// StatusOpenProofGap means it is suspected and one step short of proved.
	// The step that would close it belongs in the finding.
	StatusOpenProofGap Status = "open_proof_gap"
)

// statusNames is the whole vocabulary, in the order a report reads it.
var statusNames = []string{string(StatusConfirmed), string(StatusRuledOut), string(StatusOpenProofGap)}

// Severities, in the order a report reads them: the worst first.
const (
	SeverityInfo     = "info"
	SeverityLow      = "low"
	SeverityMedium   = "medium"
	SeverityHigh     = "high"
	SeverityCritical = "critical"
)

// severityNames is the whole severity vocabulary, worst first.
var severityNames = []string{SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow, SeverityInfo}

// severityRank orders the severities for the report and the list. A severity
// outside the vocabulary has no rank and sorts last, which is what an
// unreadable severity should do rather than sort as critical.
var severityRank = map[string]int{
	SeverityInfo:     0,
	SeverityLow:      1,
	SeverityMedium:   2,
	SeverityHigh:     3,
	SeverityCritical: 4,
}

// Finding is one item of record.
type Finding struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Severity    string `json:"severity"` // info, low, medium, high, critical
	Status      Status `json:"status"`
	URL         string `json:"url,omitempty"`
	Endpoint    string `json:"endpoint,omitempty"`
	Description string `json:"description,omitempty"`
	// PoC is the reproduction: a curl line or a script. It is the part a
	// reader copies, so it is carried verbatim and never truncated.
	PoC string `json:"poc,omitempty"`
	// Evidence is what was observed.
	Evidence string `json:"evidence,omitempty"`
	// Counterevidence is what argues against the finding. A confirmed
	// finding without it reads as a finding nobody argued with.
	Counterevidence string  `json:"counterevidence,omitempty"`
	CVSSVector      string  `json:"cvss_vector,omitempty"`
	CVSSScore       float64 `json:"cvss_score,omitempty"`
	Remediation     string  `json:"remediation,omitempty"`
	// Verdict is the verification outcome: PROVEN, REFUTED, UNPROVEN or
	// INCONCLUSIVE. It is empty when the finding was never checked, and a
	// reader can tell the difference between "not checked" and "checked and
	// proved nothing".
	Verdict string `json:"verdict,omitempty"`
	// Reasons are the sentences the verification gave for its verdict.
	Reasons   []string  `json:"reasons,omitempty"`
	Tags      []string  `json:"tags,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ParseStatus reads a closure state and refuses anything outside the
// vocabulary.
//
// The error names all three states because the caller that has to recover is
// the one that produced the value, and "invalid status" tells it nothing it
// can act on.
func ParseStatus(value string) (Status, error) {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	for _, name := range statusNames {
		if trimmed == name {
			return Status(trimmed), nil
		}
	}
	return "", fmt.Errorf("status %q is not one of %s", value, strings.Join(statusNames, ", "))
}

// ParseSeverity reads a severity and refuses anything outside the vocabulary.
func ParseSeverity(value string) (string, error) {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	for _, name := range severityNames {
		if trimmed == name {
			return trimmed, nil
		}
	}
	return "", fmt.Errorf("severity %q is not one of %s", value, strings.Join(severityNames, ", "))
}

// safeID is the shape an id may take. It is deliberately narrow: an id names
// a file inside the store, so anything with a separator, a leading dot or a
// path in it is a way out of the store rather than a name in it.
var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,119}$`)

// ValidateID refuses an id that could never name a file inside the store.
//
// The tool calls it before it looks for a record, so a model that named an id
// the store cannot hold is told that, rather than being told that the finding
// does not exist.
func ValidateID(id string) error {
	if !safeID.MatchString(strings.TrimSpace(id)) {
		return fmt.Errorf("findings: id %q is not usable; use letters, digits, dot, dash or underscore, starting with a letter or digit", id)
	}
	return nil
}

// Store holds the finding record on disk: one JSON file per finding.
//
// There is no index and no database. The record is small, the reads are whole
// files, and a store that needs a migration is a store that will not be
// migrated on the day a report is due.
type Store struct {
	mu  sync.Mutex
	dir string
}

// Open creates or opens a finding store.
//
// The directory is created 0700 and every file in it is written 0600: a
// finding record carries evidence of somebody's weakness, and the reader of
// the store is the operator, not the world.
func Open(dir string) (*Store, error) {
	trimmed := strings.TrimSpace(dir)
	if trimmed == "" {
		return nil, fmt.Errorf("findings: the store directory is empty")
	}
	if err := os.MkdirAll(trimmed, 0o700); err != nil {
		return nil, fmt.Errorf("findings: create the store %q: %w", trimmed, err)
	}
	return &Store{dir: trimmed}, nil
}

// Dir returns the directory the store lives in.
func (s *Store) Dir() string { return s.dir }

// path is the file one finding lives in.
func (s *Store) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

// Put writes a finding, replacing any finding with the same id.
//
// The write is atomic: a crash between the write and the rename cannot leave
// a half-written record that a later read mistakes for a finding.
func (s *Store) Put(f Finding) (Finding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, err := s.prepare(f)
	if err != nil {
		return Finding{}, err
	}
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return Finding{}, fmt.Errorf("findings: encode %q: %w", stored.ID, err)
	}
	if err := WriteAtomic(s.path(stored.ID), data); err != nil {
		return Finding{}, err
	}
	return stored, nil
}

// prepare validates a finding and fills in what the store owns: the id, the
// closure state, the severity and the timestamps.
func (s *Store) prepare(f Finding) (Finding, error) {
	title := strings.TrimSpace(f.Title)
	if title == "" {
		return Finding{}, fmt.Errorf("findings: a finding needs a title")
	}
	id := strings.TrimSpace(f.ID)
	if id == "" {
		id = Slug(title)
	}
	if err := ValidateID(id); err != nil {
		return Finding{}, err
	}

	status := StatusConfirmed
	if strings.TrimSpace(string(f.Status)) != "" {
		parsed, err := ParseStatus(string(f.Status))
		if err != nil {
			return Finding{}, err
		}
		status = parsed
	}
	severity := SeverityMedium
	if strings.TrimSpace(f.Severity) != "" {
		parsed, err := ParseSeverity(f.Severity)
		if err != nil {
			return Finding{}, err
		}
		severity = parsed
	}

	now := time.Now().UTC()
	created := f.CreatedAt
	if created.IsZero() {
		// An update that did not carry the original creation time keeps it
		// anyway: the record's age is part of what it says, and an upsert
		// that reset it would make every rewritten finding look new.
		if existing, ok := s.read(id); ok {
			created = existing.CreatedAt
		}
	}
	if created.IsZero() {
		created = now
	}

	return Finding{
		ID:              id,
		Title:           title,
		Severity:        severity,
		Status:          status,
		URL:             strings.TrimSpace(f.URL),
		Endpoint:        strings.TrimSpace(f.Endpoint),
		Description:     f.Description,
		PoC:             f.PoC,
		Evidence:        f.Evidence,
		Counterevidence: f.Counterevidence,
		CVSSVector:      strings.TrimSpace(f.CVSSVector),
		CVSSScore:       f.CVSSScore,
		Remediation:     f.Remediation,
		// The verdict travels with the record: a finding that was checked
		// and proved nothing is not the same as one that was never
		// checked.
		Verdict:   strings.TrimSpace(f.Verdict),
		Reasons:   f.Reasons,
		Tags:      f.Tags,
		CreatedAt: created.UTC(),
		UpdatedAt: now,
	}, nil
}

// Get reads one finding. The boolean is false when there is no such finding
// or when its file cannot be read.
func (s *Store) Get(id string) (Finding, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !safeID.MatchString(strings.TrimSpace(id)) {
		return Finding{}, false
	}
	return s.read(strings.TrimSpace(id))
}

// read loads one finding file. The caller holds the lock.
func (s *Store) read(id string) (Finding, bool) {
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return Finding{}, false
	}
	var f Finding
	if err := json.Unmarshal(data, &f); err != nil {
		return Finding{}, false
	}
	return f, true
}

// List returns every finding, worst first and then most recently touched.
//
// A file that cannot be decoded is an error, not a skip. A disclosure that
// silently drops one finding is the failure this whole package exists to
// prevent, and a corrupt record is exactly the finding a reader most needs to
// hear about.
func (s *Store) List() ([]Finding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("findings: read the store %q: %w", s.dir, err)
	}
	out := make([]Finding, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		// The rendered report lives in the same directory and is not a
		// finding; neither is a temporary file left by an interrupted write.
		if !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			return nil, fmt.Errorf("findings: read %q: %w", name, err)
		}
		var f Finding
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("findings: decode %q: %w", name, err)
		}
		out = append(out, f)
	}
	sortFindings(out)
	return out, nil
}

// sortFindings orders the record for a reader: the worst severity first, and
// within a severity the finding that moved last comes first, because it is the
// one the run was working on.
func sortFindings(list []Finding) {
	sort.SliceStable(list, func(i, j int) bool {
		left, right := severityRank[list[i].Severity], severityRank[list[j].Severity]
		if left != right {
			return left > right
		}
		if !list[i].UpdatedAt.Equal(list[j].UpdatedAt) {
			return list[i].UpdatedAt.After(list[j].UpdatedAt)
		}
		return list[i].ID < list[j].ID
	})
}

// Delete removes a finding. Removing one that is not there is an error, so
// a caller that thinks it deleted something is told when it did not.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	trimmed := strings.TrimSpace(id)
	if err := ValidateID(trimmed); err != nil {
		return err
	}
	if err := os.Remove(s.path(trimmed)); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("findings: no finding with id %q", trimmed)
		}
		return fmt.Errorf("findings: remove %q: %w", trimmed, err)
	}
	return nil
}

// Coverage summarises what the record contains.
type Coverage struct {
	Total      int
	ByStatus   map[Status]int
	BySeverity map[string]int
	// OpenGaps holds the titles of the open proof gaps.
	OpenGaps []string
	// RuledOut holds the titles of the ruled out findings.
	RuledOut []string
}

// Coverage counts the record by closure state and severity.
//
// The two title lists are the point of the whole count: a reader who sees
// "5 findings" and nothing else cannot tell whether the other four were
// ruled out or never looked at.
func (s *Store) Coverage() (Coverage, error) {
	list, err := s.List()
	if err != nil {
		return Coverage{}, err
	}
	coverage := Coverage{
		ByStatus:   map[Status]int{},
		BySeverity: map[string]int{},
	}
	for _, f := range list {
		coverage.Total++
		coverage.ByStatus[f.Status]++
		coverage.BySeverity[f.Severity]++
		switch f.Status {
		case StatusOpenProofGap:
			coverage.OpenGaps = append(coverage.OpenGaps, f.Title)
		case StatusRuledOut:
			coverage.RuledOut = append(coverage.RuledOut, f.Title)
		}
	}
	return coverage, nil
}

// WriteReport renders the record and writes report.md or report.html into the
// store directory, returning the path it wrote.
//
// The report lands beside the findings rather than in a reports directory: it
// is the same record seen from the other side, and a report that can drift
// away from the store it came from is a report nobody trusts.
func (s *Store) WriteReport(format string) (string, error) {
	list, err := s.List()
	if err != nil {
		return "", err
	}
	coverage, err := s.Coverage()
	if err != nil {
		return "", err
	}

	name := "report.md"
	body := RenderMarkdown(list, coverage)
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "md", "markdown":
	case "html":
		name = "report.html"
		body = RenderHTML(list, coverage)
	default:
		return "", fmt.Errorf("findings: report format %q is not one of md, html", format)
	}
	path := filepath.Join(s.dir, name)
	if err := WriteAtomic(path, []byte(body)); err != nil {
		return "", err
	}
	return path, nil
}

// WriteAtomic writes a file through a temporary file and a rename.
//
// The rename is what makes the write atomic: a reader either sees the old
// file or the new one, never a half-written one. The temporary file carries
// the mode as well, because a file that already exists keeps the mode it had
// and a report written 0644 once stays 0644 forever.
func WriteAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, "."+filepath.Base(path)+".tmp")
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("findings: write %q: %w", tmp, err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(tmp)
		return fmt.Errorf("findings: write %q: %w", tmp, err)
	}
	if err := file.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("findings: write %q: %w", tmp, err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("findings: secure %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("findings: replace %q: %w", path, err)
	}
	return nil
}

// Slug turns a title into an id that is safe as a file name and as a URL.
//
// The hash suffix is what keeps two titles apart. "SQLi in /admin" and "SQLi
// in /users" slug to the same words, and an id that collides would silently
// overwrite a record - the one failure a record of record cannot have.
func Slug(title string) string {
	trimmed := strings.TrimSpace(title)
	var b strings.Builder
	dashed := false
	for _, r := range strings.ToLower(trimmed) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dashed = false
		default:
			// One dash for a run of anything else: a title with punctuation
			// in it should not turn into a string of dashes.
			if !dashed && b.Len() > 0 {
				b.WriteByte('-')
				dashed = true
			}
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		base = "finding"
	}
	if len(base) > 60 {
		base = strings.Trim(base[:60], "-")
	}
	sum := sha256.Sum256([]byte(trimmed))
	return base + "-" + hex.EncodeToString(sum[:])[:8]
}
