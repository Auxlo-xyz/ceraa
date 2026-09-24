package telegram

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"sooraa/internal/config"
)

type bridgeState struct {
	mu               sync.RWMutex
	writeMu          sync.Mutex
	path             string
	Model            string                            `json:"model,omitempty"`
	Provider         string                            `json:"provider,omitempty"`
	AllowedUserID    int64                             `json:"allowed_user_id,omitempty"`
	ProviderProfiles map[string]config.ProviderProfile `json:"provider_profiles,omitempty"`
	Agent            string                            `json:"agent,omitempty"`
	Variant          string                            `json:"variant,omitempty"`
	MaxSteps         int                               `json:"max_steps,omitempty"`
	ActiveSessions   map[string]string                 `json:"active_sessions,omitempty"`
	Projects         map[string]string                 `json:"projects,omitempty"`
	SessionProjects  map[string]string                 `json:"session_projects,omitempty"`
	AlwaysAllow      map[string]bool                   `json:"always_allow,omitempty"`
	AutoCompaction   bool                              `json:"auto_compaction,omitempty"`
	AutoSet          bool                              `json:"auto_compaction_set,omitempty"`
}

var defaultState = loadBridgeState()
var forcedCompactions sync.Map

func loadBridgeState() *bridgeState {
	home, _ := os.UserHomeDir()
	return loadBridgeStateFromFile(filepath.Join(home, ".config", "sooraa", "bridge-state.json"))
}

func loadBridgeStateFromFile(path string) *bridgeState {
	state := &bridgeState{path: path, ActiveSessions: make(map[string]string), Projects: make(map[string]string), SessionProjects: make(map[string]string), AlwaysAllow: make(map[string]bool), ProviderProfiles: make(map[string]config.ProviderProfile)}
	data, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(data, state)
	}
	if state.ActiveSessions == nil {
		state.ActiveSessions = make(map[string]string)
	}
	if state.Projects == nil {
		state.Projects = make(map[string]string)
	}
	if state.SessionProjects == nil {
		state.SessionProjects = make(map[string]string)
	}
	if state.AlwaysAllow == nil {
		state.AlwaysAllow = make(map[string]bool)
	}
	if state.ProviderProfiles == nil {
		state.ProviderProfiles = make(map[string]config.ProviderProfile)
	}
	state.path = path
	return state
}

func (s *bridgeState) save() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.RLock()
	data, err := json.MarshalIndent(s, "", "  ")
	path := s.path
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bridge-state-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func (s *bridgeState) sessionID(chatID int64) string {
	key := strconv.FormatInt(chatID, 10)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if id := s.ActiveSessions[key]; id != "" {
		return id
	}
	return key
}

func (s *bridgeState) setSession(chatID int64, sessionID string) error {
	key := strconv.FormatInt(chatID, 10)
	s.mu.Lock()
	s.ActiveSessions[key] = sessionID
	if root := s.Projects[key]; root != "" {
		s.SessionProjects[sessionID] = root
	}
	s.mu.Unlock()
	return s.save()
}

func configuredWorkspaceRoot() string {
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return config.DefaultConfig().WorkspaceRootPath()
	}
	return cfg.WorkspaceRootPath()
}

func validateWorkspaceProject(root string) (string, error) {
	workspace := configuredWorkspaceRoot()
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return "", fmt.Errorf("create workspace root: %w", err)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", errors.New("project must be an existing directory")
	}
	workspaceReal, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	projectReal, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve project: %w", err)
	}
	rel, err := filepath.Rel(workspaceReal, projectReal)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("project must be inside workspace root %s", workspace)
	}
	return abs, nil
}

func (s *bridgeState) project(chatID int64) string {
	key := strconv.FormatInt(chatID, 10)
	s.mu.RLock()
	value := s.Projects[key]
	s.mu.RUnlock()
	if value == "" {
		return ""
	}
	valid, err := validateWorkspaceProject(value)
	if err != nil {
		return ""
	}
	return valid
}

func (s *bridgeState) setProject(chatID int64, root string) error {
	abs, err := validateWorkspaceProject(root)
	if err != nil {
		return err
	}
	key := strconv.FormatInt(chatID, 10)
	s.mu.Lock()
	s.Projects[key] = abs
	sessionID := s.ActiveSessions[key]
	if sessionID == "" {
		sessionID = key
	}
	s.SessionProjects[sessionID] = abs
	s.mu.Unlock()
	return s.save()
}

func (s *bridgeState) projectForSession(sessionID string) string {
	s.mu.RLock()
	root := s.SessionProjects[sessionID]
	s.mu.RUnlock()
	if root == "" {
		return ""
	}
	valid, err := validateWorkspaceProject(root)
	if err != nil {
		return ""
	}
	return valid
}

func (s *bridgeState) setAutoCompaction(enabled bool) error {
	s.mu.Lock()
	s.AutoCompaction = enabled
	s.AutoSet = true
	s.mu.Unlock()
	return s.save()
}

func (s *bridgeState) autoCompaction() (bool, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.AutoCompaction, s.AutoSet
}

func (s *bridgeState) setAlwaysAllow(toolName string, allowed bool) error {
	s.mu.Lock()
	if allowed {
		s.AlwaysAllow[toolName] = true
	} else {
		delete(s.AlwaysAllow, toolName)
	}
	s.mu.Unlock()
	return s.save()
}

func (s *bridgeState) alwaysAllowed(toolName string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.AlwaysAllow[toolName]
}

func (s *bridgeState) values() (string, string, string, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Model, s.Agent, s.Variant, s.MaxSteps
}

func (s *bridgeState) provider() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.Provider == "" {
		return "default"
	}
	return s.Provider
}

func (s *bridgeState) allowedUserID() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.AllowedUserID
}

func (s *bridgeState) setAllowedUserID(id int64) error {
	if id <= 0 {
		return errors.New("Telegram user ID must be positive")
	}
	s.mu.Lock()
	s.AllowedUserID = id
	s.mu.Unlock()
	return s.save()
}

func (s *bridgeState) providerProfiles() map[string]config.ProviderProfile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	profiles := make(map[string]config.ProviderProfile, len(s.ProviderProfiles))
	for key, profile := range s.ProviderProfiles {
		profiles[key] = profile
	}
	return profiles
}

func (s *bridgeState) setProvider(provider string) error {
	if provider == "" {
		provider = "default"
	}
	s.mu.Lock()
	s.Provider = provider
	s.mu.Unlock()
	return s.save()
}

func (s *bridgeState) setProviderProfile(name string, profile config.ProviderProfile) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("provider name is empty")
	}
	s.mu.Lock()
	s.ProviderProfiles[name] = profile
	s.mu.Unlock()
	return s.save()
}

func (s *bridgeState) setModel(model string) error {
	s.mu.Lock()
	s.Model = model
	s.mu.Unlock()
	if err := os.Setenv("SOORAA_MODEL", model); err != nil {
		return err
	}
	return s.save()
}

func (s *bridgeState) setAgent(agent string) error {
	s.mu.Lock()
	s.Agent = agent
	s.mu.Unlock()
	return s.save()
}

func (s *bridgeState) setVariant(variant string) error {
	s.mu.Lock()
	s.Variant = variant
	s.mu.Unlock()
	return s.save()
}

func (s *bridgeState) setMaxSteps(steps int) error {
	if steps < 1 || steps > 50 {
		return errors.New("max steps must be between 1 and 50")
	}
	s.mu.Lock()
	s.MaxSteps = steps
	s.mu.Unlock()
	if err := os.Setenv("SOORAA_MAX_STEPS", strconv.Itoa(steps)); err != nil {
		return err
	}
	return s.save()
}

func requestForcedCompaction(sessionID string) {
	forcedCompactions.Store(sessionID, true)
}

func consumeForcedCompaction(sessionID string) bool {
	_, loaded := forcedCompactions.LoadAndDelete(sessionID)
	return loaded
}
