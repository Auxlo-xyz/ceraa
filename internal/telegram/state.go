package telegram

import (
	"ceraa/harness/llm"
	"ceraa/internal/config"
	"ceraa/internal/envs"
	"ceraa/internal/paths"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

type bridgeState struct {
	mu               sync.RWMutex
	writeMu          sync.Mutex
	path             string
	loadErr          error
	Model            string                            `json:"model,omitempty"`
	Provider         string                            `json:"provider,omitempty"`
	AllowedUserID    int64                             `json:"allowed_user_id,omitempty"`
	ProviderProfiles map[string]config.ProviderProfile `json:"provider_profiles,omitempty"`
	Agent            string                            `json:"agent,omitempty"`
	Variant          string                            `json:"variant,omitempty"`
	Reasoning        string                            `json:"reasoning,omitempty"`
	answerRequests   map[string]int64
	ActiveSessions   map[string]string `json:"active_sessions,omitempty"`
	Projects         map[string]string `json:"projects,omitempty"`
	SessionProjects  map[string]string `json:"session_projects,omitempty"`
	AlwaysAllow      map[string]bool   `json:"always_allow,omitempty"`
	AutoCompaction   bool              `json:"auto_compaction,omitempty"`
	AutoSet          bool              `json:"auto_compaction_set,omitempty"`
}

var defaultState = loadBridgeState()
var forcedCompactions sync.Map

func loadBridgeState() *bridgeState {
	return loadBridgeStateFromFile(filepath.Join(paths.DataDir(), "bridge-state.json"))
}

func loadBridgeStateFromFile(path string) *bridgeState {
	state := &bridgeState{path: path, ActiveSessions: make(map[string]string), Projects: make(map[string]string), SessionProjects: make(map[string]string), AlwaysAllow: make(map[string]bool), ProviderProfiles: make(map[string]config.ProviderProfile)}
	data, err := os.ReadFile(path)
	if err == nil {
		if unmarshalErr := json.Unmarshal(data, state); unmarshalErr != nil {
			state.loadErr = fmt.Errorf("parse bridge state: %w", unmarshalErr)
		}
	} else if !os.IsNotExist(err) {
		state.loadErr = fmt.Errorf("read bridge state: %w", err)
	}
	if state.AllowedUserID < 0 {
		state.loadErr = errors.New("bridge state allowed user ID must be positive")
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
		return configuredWorkspaceRoot()
	}
	valid, err := validateWorkspaceProject(value)
	if err != nil {
		return configuredWorkspaceRoot()
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
		return configuredWorkspaceRoot()
	}
	valid, err := validateWorkspaceProject(root)
	if err != nil {
		return configuredWorkspaceRoot()
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

func (s *bridgeState) values() (string, string, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Model, s.Agent, s.Variant
}

// reasoning returns the runtime reasoning level, empty when the provider's own
// default should apply.
func (s *bridgeState) reasoning() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Reasoning
}

// setReasoning records the runtime reasoning level. An empty value clears it and
// restores the provider default.
func (s *bridgeState) setReasoning(level string) error {
	level = strings.TrimSpace(level)
	if level != "" && !llm.ReasonLevel(level).Valid() {
		return errors.New("unknown reasoning level " + level)
	}
	s.mu.Lock()
	s.Reasoning = level
	s.mu.Unlock()
	return s.save()
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

func (s *bridgeState) claimAllowedUserID(id int64) (bool, error) {
	if id <= 0 {
		return false, errors.New("Telegram user ID must be positive")
	}
	s.mu.Lock()
	if s.AllowedUserID != 0 && s.AllowedUserID != id {
		s.mu.Unlock()
		return false, nil
	}
	s.AllowedUserID = id
	s.mu.Unlock()
	if err := s.save(); err != nil {
		return false, err
	}
	return true, nil
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

// removeProviderProfile deletes a stored profile, reporting whether one was there.
//
// "default" is refused: it is not a stored profile but the top-level provider
// settings, so deleting it would have nothing to remove and would leave the
// bot with no provider at all.
func (s *bridgeState) removeProviderProfile(name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, errors.New("provider name is empty")
	}
	if name == "default" {
		return false, errors.New("the default provider is the base configuration and cannot be removed; switch to another provider instead")
	}
	s.mu.Lock()
	if _, ok := s.ProviderProfiles[name]; !ok {
		s.mu.Unlock()
		return false, errors.New("no provider named " + name)
	}
	delete(s.ProviderProfiles, name)
	// Removing the active provider must not leave the bot pointing at a profile
	// that no longer exists, so fall back to default.
	if s.Provider == name {
		s.Provider = "default"
	}
	s.mu.Unlock()
	return true, s.save()
}

// activeProviderProfile returns the profile backing the active provider, and
// whether one was found.
//
// The runtime model override is deliberately not consulted here: it is empty
// unless the user set one, so reading it alone reports a blank model and hides
// which model is actually answering.
func (s *bridgeState) activeProviderProfile() (string, config.ProviderProfile, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id := s.Provider
	if id == "" {
		id = "default"
	}
	if profile, ok := s.ProviderProfiles[id]; ok {
		return id, profile, true
	}
	return id, config.ProviderProfile{}, false
}

func (s *bridgeState) setModel(model string) error {
	s.mu.Lock()
	s.Model = model
	s.mu.Unlock()
	if err := envs.Set("MODEL", model); err != nil {
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

func requestForcedCompaction(sessionID string) {
	forcedCompactions.Store(sessionID, true)
}

func consumeForcedCompaction(sessionID string) bool {
	_, loaded := forcedCompactions.LoadAndDelete(sessionID)
	return loaded
}

// awaitingAnswer records that the user's next plain message answers a question
// rather than starting a new task. Without this, a typed answer would be run as
// if it were a prompt.
func (s *bridgeState) expectAnswer(requestID string, chatID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.answerRequests == nil {
		s.answerRequests = make(map[string]int64)
	}
	s.answerRequests[requestID] = chatID
}

// takeAnswer returns the chat a typed answer belongs to, and clears it so a
// second message is treated as a task again.
func (s *bridgeState) takeAnswer(chatID int64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for requestID, owner := range s.answerRequests {
		if owner == chatID {
			delete(s.answerRequests, requestID)
			return requestID, true
		}
	}
	return "", false
}
