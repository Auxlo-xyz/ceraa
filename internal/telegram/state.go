package telegram

import (
	"ceraa/harness/llm"
	"ceraa/harness/tool"
	"ceraa/internal/config"
	"ceraa/internal/logger"
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

// bridgeState is the bot's own runtime state: who is allowed to talk to it, which
// chat is bound to which session, which project each session works in, and which
// tools are always allowed.
//
// It deliberately holds no provider, model, reasoning or agent selection. Those
// used to be fields here, duplicating config.json, and two files with two
// precedence rules is how the menu and the run path ended up disagreeing about
// which model was serving. They live in config.json and are reached through
// providerState.
type bridgeState struct {
	mu             sync.RWMutex
	writeMu        sync.Mutex
	path           string
	loadErr        error
	AllowedUserID  int64 `json:"allowed_user_id,omitempty"`
	answerRequests map[string]int64
	// pendingUploads holds files the user sent since the last run, oldest
	// first, keyed by chat. A save notice goes to the chat as a message, which
	// the next run never sees, so without this "check that file out" resolves
	// against stale conversation instead of the file that just arrived.
	// Runtime only, never persisted: a restart drops them, and the user can
	// simply name the file again.
	pendingUploads  map[int64][]uploadNote
	ActiveSessions  map[string]string `json:"active_sessions,omitempty"`
	Projects        map[string]string `json:"projects,omitempty"`
	SessionProjects map[string]string `json:"session_projects,omitempty"`
	AlwaysAllow     map[string]bool   `json:"always_allow,omitempty"`
	// UpdateCheck persists the release-check preference. Nil means enabled, so
	// an install that predates the setting behaves as it always did.
	UpdateCheck *bool `json:"update_check,omitempty"`
}

var defaultState = loadBridgeState()
var forcedCompactions sync.Map

func loadBridgeState() *bridgeState {
	return loadBridgeStateFromFile(filepath.Join(paths.DataDir(), "bridge-state.json"))
}

func loadBridgeStateFromFile(path string) *bridgeState {
	state := &bridgeState{path: path, ActiveSessions: make(map[string]string), Projects: make(map[string]string), SessionProjects: make(map[string]string), AlwaysAllow: make(map[string]bool)}
	data, err := os.ReadFile(path)
	// Whether the file names the allow list at all. The in-memory map starts
	// non-nil, so its nilness after load says nothing: only the raw bytes
	// tell a fresh state (or a pre-list one) apart from an explicit empty one.
	allowKeyPresent := false
	if err == nil {
		var probe struct {
			AlwaysAllow map[string]bool `json:"always_allow"`
		}
		if json.Unmarshal(data, &probe) == nil && probe.AlwaysAllow != nil {
			allowKeyPresent = true
		}
	}
	if err == nil {
		if unmarshalErr := json.Unmarshal(data, state); unmarshalErr != nil {
			state.loadErr = fmt.Errorf("parse bridge state: %w", unmarshalErr)
		}
		// Fold a pre-consolidation selection into config.json before dropping it.
		// Migrating on read rather than at upgrade time means an install that was
		// last written by an older build is not stranded: whichever process starts
		// first performs the move, and it is idempotent.
		if legacy, found, migErr := readLegacySelection(data); found && migErr == nil {
			if err := adoptLegacySelection(legacy); err != nil {
				state.loadErr = fmt.Errorf("migrate bridge state: %w", err)
			}
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
	if !allowKeyPresent {
		// Never configured: a fresh state, or one written before the list
		// existed. Default every registry tool to allowed and persist, so
		// the choice reads as explicit from here on. A user who turns tools
		// off later keeps an explicit (possibly empty) map, which is never
		// reseeded: presence of the key, not its size, is what counts.
		// A state that failed to load is left alone rather than seeded and
		// saved, so a corrupt file is never paved over by defaults.
		if state.loadErr == nil {
			if state.AlwaysAllow == nil {
				state.AlwaysAllow = make(map[string]bool)
			}
			for _, t := range tool.DefaultRegistryWithStores(nil).All() {
				state.AlwaysAllow[t.Name()] = true
			}
			state.path = path
			if err := state.save(); err != nil {
				logger.Warn("allow-list seed not persisted: " + err.Error())
			}
		} else if state.AlwaysAllow == nil {
			state.AlwaysAllow = make(map[string]bool)
		}
	} else if state.AlwaysAllow == nil {
		state.AlwaysAllow = make(map[string]bool)
	}
	state.path = path
	return state
}

// legacySelection is the provider and model state an older bridge-state.json held.
type legacySelection struct {
	Provider         string                            `json:"provider"`
	Model            string                            `json:"model"`
	Agent            string                            `json:"agent"`
	Reasoning        string                            `json:"reasoning"`
	ProviderProfiles map[string]config.ProviderProfile `json:"provider_profiles"`
	AutoCompaction   *bool                             `json:"auto_compaction"`
}

// readLegacySelection reports whether an old bridge-state.json carried a selection
// this build should adopt.
//
// The test is the identifying key, not the full retired set. An install only ever
// wrote the fields the user had actually set, so requiring all seven rejected the
// exact file this migration exists for: a live bridge-state holding only
// `provider`, `provider_profiles` and `agent` looked like a new file and the
// selection was silently dropped, leaving the bot on the default provider.
func readLegacySelection(data []byte) (legacySelection, bool, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return legacySelection{}, false, err
	}
	_, hasProvider := raw["provider"]
	_, hasProfiles := raw["provider_profiles"]
	if !hasProvider && !hasProfiles {
		return legacySelection{}, false, nil
	}
	var legacy legacySelection
	if err := json.Unmarshal(data, &legacy); err != nil {
		return legacySelection{}, false, err
	}
	return legacy, true, nil
}

// adoptLegacySelection writes a pre-consolidation selection into config.json.
//
// It only fills values config does not already have, so re-running it can never
// clobber a choice the user has since made through the consolidated path.
func adoptLegacySelection(legacy legacySelection) error {
	return providers.store.Update(func(cfg *config.Config) error {
		if strings.TrimSpace(cfg.ActiveProvider) == "" && strings.TrimSpace(legacy.Provider) != "" {
			cfg.ActiveProvider = strings.TrimSpace(legacy.Provider)
		}
		if strings.TrimSpace(cfg.Model) == "" && strings.TrimSpace(legacy.Model) != "" {
			cfg.Model = strings.TrimSpace(legacy.Model)
		}
		if strings.TrimSpace(cfg.Agent) == "" && strings.TrimSpace(legacy.Agent) != "" {
			cfg.Agent = strings.TrimSpace(legacy.Agent)
		}
		if strings.TrimSpace(cfg.Reasoning) == "" && strings.TrimSpace(legacy.Reasoning) != "" {
			cfg.Reasoning = strings.TrimSpace(legacy.Reasoning)
		}
		if cfg.AutoCompaction == nil && legacy.AutoCompaction != nil {
			cfg.AutoCompaction = legacy.AutoCompaction
		}
		for name, profile := range legacy.ProviderProfiles {
			if cfg.ProviderProfiles == nil {
				cfg.ProviderProfiles = map[string]config.ProviderProfile{}
			}
			if _, exists := cfg.ProviderProfiles[name]; !exists {
				cfg.ProviderProfiles[name] = profile
			}
		}
		return nil
	})
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

// activeSessionIDs snapshots the chat to session binding for background
// sweeps. A copy rather than the live map, so a sweep never holds the state
// lock across session I/O and a /new mid-sweep is picked up next pass.
func (s *bridgeState) activeSessionIDs() map[int64]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[int64]string, len(s.ActiveSessions))
	for key, sessionID := range s.ActiveSessions {
		chatID, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			continue
		}
		out[chatID] = sessionID
	}
	return out
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
	return providers.setAutoCompaction(enabled)
}

func (s *bridgeState) autoCompaction() (bool, bool) {
	return providers.autoCompaction()
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

// updateCheckEnabled reports whether the startup release check may run. The
// environment override is consulted by the caller, not here: this answers
// what the file says, and the caller layers the machine policy over it.
func (s *bridgeState) updateCheckEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.UpdateCheck == nil || *s.UpdateCheck
}

func (s *bridgeState) setUpdateCheck(enabled bool) error {
	s.mu.Lock()
	s.UpdateCheck = &enabled
	s.mu.Unlock()
	return s.save()
}

// values reports the runtime model and agent mode.
//
// Two values rather than three: the third used to be the response variant, a
// compaction tuning knob presented as a writing style, and it was removed.
func (s *bridgeState) values() (string, string) {
	return providers.modelAndAgent()
}

// reasoning returns the runtime reasoning level, empty when the provider's own
// default should apply.
func (s *bridgeState) reasoning() string {
	return providers.reasoning()
}

// setReasoning records the runtime reasoning level. An empty value clears it and
// restores the provider default.
func (s *bridgeState) setReasoning(level string) error {
	level = strings.TrimSpace(level)
	if level != "" && !llm.ReasonLevel(level).Valid() {
		return errors.New("unknown reasoning level " + level)
	}
	return providers.setReasoning(level)
}

func (s *bridgeState) provider() string {
	return providers.active()
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
	return providers.profiles()
}

func (s *bridgeState) setProvider(provider string) error {
	return providers.setActive(provider)
}

func (s *bridgeState) setProviderProfile(name string, profile config.ProviderProfile) error {
	return providers.setProfile(name, profile)
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
	return providers.removeProfile(name)
}

// activeProviderProfile returns the profile backing the active provider, and
// whether one was found.
//
// The runtime model override is deliberately not consulted here: it is empty
// unless the user set one, so reading it alone reports a blank model and hides
// which model is actually answering.
func (s *bridgeState) activeProviderProfile() (string, config.ProviderProfile, bool) {
	return providers.activeProfile()
}

func (s *bridgeState) setModel(model string) error {
	return providers.setModel(model)
}

func (s *bridgeState) setAgent(agent string) error {
	return providers.setAgent(agent)
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

// uploadContext renders waiting uploads as a prompt prefix. It names the
// saved path every time, because the run needs something it can hand to read,
// not just the knowledge that a file exists.
func uploadContext(chatID int64) string {
	notes := defaultState.takeUploads(chatID)
	if len(notes) == 0 {
		return ""
	}
	var lines []string
	for _, note := range notes {
		line := note.kind + " " + note.path
		if note.name != "" {
			line = note.kind + " " + note.name + " saved at " + note.path
		}
		if note.kind == "photo" {
			line += " (read the file to see it)"
		}
		if note.kind == "voice" {
			line += " (transcription is not available; ask the user for text if you need its content)"
		}
		lines = append(lines, "- "+line)
	}
	context := "[Files the user sent since your last reply, oldest first. " +
		"The last one is the most recent, and a bare \"that file\" or \"it\" " +
		"in the message below most plausibly means it unless the user names " +
		"something else.]\n" + strings.Join(lines, "\n")
	return context
}

// withUploadContext prefixes a prompt with waiting uploads, if any.
func withUploadContext(chatID int64, prompt string) string {
	if context := uploadContext(chatID); context != "" {
		return context + "\n\n" + prompt
	}
	return prompt
}

// uploadNote is one file the user sent that no run has seen yet.
type uploadNote struct {
	// kind is photo, document or voice, the words the save notice used.
	kind string
	// path is where the file was saved on disk.
	path string
	// name is the sender's filename, empty for photos and voice notes.
	name string
}

// maxPendingUploads bounds the notes kept per chat. Uploads without a
// following prompt are abandoned, and an unbounded list would let them pile up
// across a session of pure file drops.
const maxPendingUploads = 5

// noteUpload records that a file arrived and is waiting for a prompt.
func (s *bridgeState) noteUpload(chatID int64, kind, path, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingUploads == nil {
		s.pendingUploads = make(map[int64][]uploadNote)
	}
	notes := append(s.pendingUploads[chatID], uploadNote{kind: kind, path: path, name: name})
	if len(notes) > maxPendingUploads {
		notes = notes[len(notes)-maxPendingUploads:]
	}
	s.pendingUploads[chatID] = notes
}

// takeUploads returns the waiting files, oldest first, and clears them so the
// next prompt is not told about the same upload twice.
func (s *bridgeState) takeUploads(chatID int64) []uploadNote {
	s.mu.Lock()
	defer s.mu.Unlock()
	notes := s.pendingUploads[chatID]
	delete(s.pendingUploads, chatID)
	return notes
}
