package skills

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"sooraa/harness/flock"
)

const schemaVersion = 1

var ErrProposalNotPending = errors.New("proposal is not pending")

type ProposalStatus string

const (
	Pending  ProposalStatus = "pending"
	Approved ProposalStatus = "approved"
	Rejected ProposalStatus = "rejected"
)

type Version struct {
	Version    int       `json:"version"`
	Content    string    `json:"content"`
	ProposalID string    `json:"proposal_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type Skill struct {
	Name      string    `json:"name"`
	Versions  []Version `json:"versions"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Proposal struct {
	ID         string         `json:"id"`
	SkillName  string         `json:"skill_name"`
	Content    string         `json:"content"`
	Feedback   string         `json:"feedback"`
	Status     ProposalStatus `json:"status"`
	Version    int            `json:"version,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	ResolvedAt *time.Time     `json:"resolved_at,omitempty"`
}

type Store struct {
	mu   sync.Mutex
	dir  string
	path string
}

type document struct {
	Version   int        `json:"version"`
	Skills    []Skill    `json:"skills"`
	Proposals []Proposal `json:"proposals"`
}

func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create skills store: %w", err)
	}
	return &Store{dir: dir, path: filepath.Join(dir, "skills.json")}, nil
}

func (s *Store) Propose(skillName, content, feedback string) (*Proposal, error) {
	skillName = strings.TrimSpace(skillName)
	content = strings.TrimSpace(content)
	feedback = strings.TrimSpace(feedback)
	if skillName == "" {
		return nil, fmt.Errorf("skill name is required")
	}
	if content == "" {
		return nil, fmt.Errorf("proposed skill content is required")
	}
	if feedback == "" {
		return nil, fmt.Errorf("proposal feedback is required")
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := flock.Acquire(filepath.Join(s.dir, ".skills.lock"))
	if err != nil {
		return nil, fmt.Errorf("lock skills store: %w", err)
	}
	defer lock.Release()

	doc, err := load(s.path)
	if err != nil {
		return nil, err
	}
	proposal := Proposal{
		ID:        id,
		SkillName: skillName,
		Content:   content,
		Feedback:  feedback,
		Status:    Pending,
		CreatedAt: time.Now().UTC(),
	}
	doc.Proposals = append(doc.Proposals, proposal)
	if err := save(s.path, doc); err != nil {
		return nil, err
	}
	return &proposal, nil
}

func (s *Store) ListProposals() ([]Proposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := flock.Acquire(filepath.Join(s.dir, ".skills.lock"))
	if err != nil {
		return nil, fmt.Errorf("lock skills store: %w", err)
	}
	defer lock.Release()

	doc, err := load(s.path)
	if err != nil {
		return nil, err
	}
	return append([]Proposal(nil), doc.Proposals...), nil
}

func (s *Store) Approve(proposalID string) (*Skill, error) {
	proposalID = strings.TrimSpace(proposalID)
	if proposalID == "" {
		return nil, fmt.Errorf("proposal ID is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := flock.Acquire(filepath.Join(s.dir, ".skills.lock"))
	if err != nil {
		return nil, fmt.Errorf("lock skills store: %w", err)
	}
	defer lock.Release()

	doc, err := load(s.path)
	if err != nil {
		return nil, err
	}
	proposalIndex := proposalIndex(doc.Proposals, proposalID)
	if proposalIndex < 0 {
		return nil, fmt.Errorf("approve proposal %s: %w", proposalID, os.ErrNotExist)
	}
	proposal := &doc.Proposals[proposalIndex]
	if proposal.Status != Pending {
		return nil, fmt.Errorf("approve proposal %s: %w", proposalID, ErrProposalNotPending)
	}

	now := time.Now().UTC()
	skillIndex := skillIndex(doc.Skills, proposal.SkillName)
	if skillIndex < 0 {
		doc.Skills = append(doc.Skills, Skill{
			Name:      proposal.SkillName,
			Versions:  []Version{},
			CreatedAt: now,
			UpdatedAt: now,
		})
		skillIndex = len(doc.Skills) - 1
	}
	skill := &doc.Skills[skillIndex]
	version := latestVersion(*skill) + 1
	skill.Versions = append(skill.Versions, Version{
		Version:    version,
		Content:    proposal.Content,
		ProposalID: proposal.ID,
		CreatedAt:  now,
	})
	skill.UpdatedAt = now
	proposal.Status = Approved
	proposal.Version = version
	proposal.ResolvedAt = &now
	if err := save(s.path, doc); err != nil {
		return nil, err
	}
	result := *skill
	result.Versions = append([]Version(nil), skill.Versions...)
	return &result, nil
}

func (s *Store) Reject(proposalID string) error {
	proposalID = strings.TrimSpace(proposalID)
	if proposalID == "" {
		return fmt.Errorf("proposal ID is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := flock.Acquire(filepath.Join(s.dir, ".skills.lock"))
	if err != nil {
		return fmt.Errorf("lock skills store: %w", err)
	}
	defer lock.Release()

	doc, err := load(s.path)
	if err != nil {
		return err
	}
	index := proposalIndex(doc.Proposals, proposalID)
	if index < 0 {
		return fmt.Errorf("reject proposal %s: %w", proposalID, os.ErrNotExist)
	}
	proposal := &doc.Proposals[index]
	if proposal.Status != Pending {
		return fmt.Errorf("reject proposal %s: %w", proposalID, ErrProposalNotPending)
	}
	now := time.Now().UTC()
	proposal.Status = Rejected
	proposal.ResolvedAt = &now
	return save(s.path, doc)
}

func (s *Store) AddVersion(skillName, content string) (*Skill, error) {
	skillName = strings.TrimSpace(skillName)
	content = strings.TrimSpace(content)
	if skillName == "" {
		return nil, fmt.Errorf("skill name is required")
	}
	if content == "" {
		return nil, fmt.Errorf("skill content is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := flock.Acquire(filepath.Join(s.dir, ".skills.lock"))
	if err != nil {
		return nil, fmt.Errorf("lock skills store: %w", err)
	}
	defer lock.Release()

	doc, err := load(s.path)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	index := skillIndex(doc.Skills, skillName)
	if index < 0 {
		doc.Skills = append(doc.Skills, Skill{
			Name:      skillName,
			Versions:  []Version{},
			CreatedAt: now,
			UpdatedAt: now,
		})
		index = len(doc.Skills) - 1
	}
	skill := &doc.Skills[index]
	skill.Versions = append(skill.Versions, Version{
		Version:   latestVersion(*skill) + 1,
		Content:   content,
		CreatedAt: now,
	})
	skill.UpdatedAt = now
	if err := save(s.path, doc); err != nil {
		return nil, err
	}
	result := *skill
	result.Versions = append([]Version(nil), skill.Versions...)
	return &result, nil
}

func (s *Store) Current(name string) (*Skill, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("skill name is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := flock.Acquire(filepath.Join(s.dir, ".skills.lock"))
	if err != nil {
		return nil, fmt.Errorf("lock skills store: %w", err)
	}
	defer lock.Release()

	doc, err := load(s.path)
	if err != nil {
		return nil, err
	}
	index := skillIndex(doc.Skills, name)
	if index < 0 {
		return nil, fmt.Errorf("current skill %s: %w", name, os.ErrNotExist)
	}
	skill := doc.Skills[index]
	skill.Versions = append([]Version(nil), skill.Versions...)
	return &skill, nil
}

func (s *Store) Rollback(name string, versionNumber int) (*Skill, error) {
	name = strings.TrimSpace(name)
	if name == "" || versionNumber < 1 {
		return nil, fmt.Errorf("skill name and version are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := flock.Acquire(filepath.Join(s.dir, ".skills.lock"))
	if err != nil {
		return nil, fmt.Errorf("lock skills store: %w", err)
	}
	defer lock.Release()
	doc, err := load(s.path)
	if err != nil {
		return nil, err
	}
	index := skillIndex(doc.Skills, name)
	if index < 0 {
		return nil, fmt.Errorf("skill %s: %w", name, os.ErrNotExist)
	}
	skill := &doc.Skills[index]
	var target *Version
	for i := range skill.Versions {
		if skill.Versions[i].Version == versionNumber {
			target = &skill.Versions[i]
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("skill %s version %d: %w", name, versionNumber, os.ErrNotExist)
	}
	now := time.Now().UTC()
	skill.Versions = append(skill.Versions, Version{
		Version:    latestVersion(*skill) + 1,
		Content:    target.Content,
		ProposalID: target.ProposalID,
		CreatedAt:  now,
	})
	skill.UpdatedAt = now
	if err := save(s.path, doc); err != nil {
		return nil, err
	}
	result := *skill
	result.Versions = append([]Version(nil), skill.Versions...)
	return &result, nil
}

func (s *Store) ActiveContext(maxBytes int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := flock.Acquire(filepath.Join(s.dir, ".skills.lock"))
	if err != nil {
		return "", fmt.Errorf("lock skills store: %w", err)
	}
	defer lock.Release()

	doc, err := load(s.path)
	if err != nil {
		return "", err
	}
	return RenderActiveContext(doc.Skills, maxBytes), nil
}

func RenderActiveContext(skillList []Skill, maxBytes int) string {
	if maxBytes <= 0 || len(skillList) == 0 {
		return ""
	}
	sorted := append([]Skill(nil), skillList...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var out strings.Builder
	out.WriteString("<skills>\n")
	for _, skill := range sorted {
		version := latestVersion(skill)
		var content string
		for i := len(skill.Versions) - 1; i >= 0; i-- {
			if skill.Versions[i].Version == version {
				content = skill.Versions[i].Content
				break
			}
		}
		item := fmt.Sprintf("<skill name=%q version=%d>%s</skill>\n", html.EscapeString(skill.Name), version, html.EscapeString(content))
		if out.Len()+len(item)+len("</skills>\n") <= maxBytes {
			out.WriteString(item)
		}
	}
	out.WriteString("</skills>\n")
	if out.Len() <= maxBytes {
		return out.String()
	}
	return truncateUTF8(out.String(), maxBytes)
}

func load(path string) (*document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &document{Version: schemaVersion, Skills: []Skill{}, Proposals: []Proposal{}}, nil
		}
		return nil, fmt.Errorf("read skills store: %w", err)
	}
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("decode skills store: %w", err)
	}
	if doc.Version != 0 && doc.Version != schemaVersion {
		return nil, fmt.Errorf("unsupported skills store version %d", doc.Version)
	}
	if doc.Skills == nil {
		doc.Skills = []Skill{}
	}
	if doc.Proposals == nil {
		doc.Proposals = []Proposal{}
	}
	return &doc, nil
}

func save(path string, doc *document) error {
	doc.Version = schemaVersion
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode skills store: %w", err)
	}
	return atomicWrite(path, data)
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".skills.json.tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary skills store: %w", err)
	}
	tmpPath := tmp.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set skills store permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary skills store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary skills store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary skills store: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace skills store: %w", err)
	}
	remove = false
	if directory, err := os.Open(dir); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func skillIndex(skillList []Skill, name string) int {
	for i := range skillList {
		if skillList[i].Name == name {
			return i
		}
	}
	return -1
}

func proposalIndex(proposals []Proposal, id string) int {
	for i := range proposals {
		if proposals[i].ID == id {
			return i
		}
	}
	return -1
}

func latestVersion(skill Skill) int {
	version := 0
	for _, candidate := range skill.Versions {
		if candidate.Version > version {
			version = candidate.Version
		}
	}
	return version
}

func randomID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("create proposal ID: %w", err)
	}
	return hex.EncodeToString(data), nil
}

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
