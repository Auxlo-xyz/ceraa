package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"soraa-agent/internal/llm"
)

type Operation struct {
	ID       string         `json:"id"`
	Tool     string         `json:"tool"`
	Args     map[string]any `json:"args"`
	Status   string         `json:"status"` // pending, running, completed, failed
	Output   string         `json:"output"`
	Error    string         `json:"error"`
	ThreadID string         `json:"thread_id"`
}

type Session struct {
	ID         string        `json:"id"`
	Messages   []llm.Message `json:"messages"`
	Operations []Operation   `json:"operations"`
}

func Load(id string) (*Session, error) {
	dir := "sessions"
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		_ = os.Mkdir(dir, 0755)
	}
	path := filepath.Join(dir, id+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		return &Session{ID: id, Messages: []llm.Message{}}, nil
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *Session) Save() error {
	dir := "sessions"
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		_ = os.Mkdir(dir, 0755)
	}
	path := filepath.Join(dir, s.ID+".json")
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0644)
}

func (s *Session) Clear() error {
	s.Messages = []llm.Message{}
	return s.Save()
}
