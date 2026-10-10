package config

import (
	"ceraa/internal/envs"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"ceraa/internal/paths"
)

// env reads a Ceraa setting from the environment; see internal/envs for the
// single definition of the prefix.
func env(name string) string {
	return envs.Get(name)
}

// Config holds all Ceraa settings.
type Config struct {
	LLM              LLMProvider                `json:"llm"`
	ProviderProfiles map[string]ProviderProfile `json:"provider_profiles,omitempty"`
	ActiveProvider   string                     `json:"active_provider,omitempty"`
	// Model, Reasoning, Agent and AutoCompaction are the model-facing settings the
	// Telegram menu edits. They used to live in bridge-state.json, which made config
	// have two sources of truth for one question: the menu and the run path read
	// "which model is this" from different files under different precedence rules,
	// so a provider switched from Telegram was not the provider the next run used.
	// They belong beside ActiveProvider, which is the same kind of decision.
	Model                    string         `json:"model,omitempty"`
	Reasoning                string         `json:"reasoning,omitempty"`
	Agent                    string         `json:"agent,omitempty"`
	AutoCompaction           *bool          `json:"auto_compaction,omitempty"`
	Subscriptions            Subscriptions  `json:"subscriptions,omitempty"`
	Telegram                 TelegramConfig `json:"telegram,omitempty"`
	SessionFile              string         `json:"session_file,omitempty"`
	WorkspaceRoot            string         `json:"workspace_root,omitempty"`
	ToolOutputDir            string         `json:"tool_output_dir,omitempty"`
	MaxToolResultChars       int            `json:"max_tool_result_chars,omitempty"`
	MaxConcurrentTools       int            `json:"max_concurrent_tools,omitempty"`
	ModelCatalogURL          string         `json:"model_catalog_url,omitempty"`
	ModelCatalogCache        string         `json:"model_catalog_cache,omitempty"`
	ModelCatalogRefreshHours int            `json:"model_catalog_refresh_hours,omitempty"`
	TitleAPIURL              string         `json:"title_api_url,omitempty"`
	TitleModel               string         `json:"title_model,omitempty"`
	MemoryFile               string         `json:"memory_file,omitempty"`
	// MemoryDelivery selects where memory reaches the model: "cached" (default)
	// renders a bounded block in the stable, cacheable prefix so a preference is
	// visible on every turn at cache-hit cost; "prompt" renders the same block
	// in the varying tail; "sidekick" annotates tool results instead and leaves
	// the system prompt fully static.
	//
	// The default is NOT safe, and an earlier version of this comment said it was
	// on the strength of a claim that the block was stable, which was false. The
	// renderer re-reads BOND.md and re-ranks the store from disk on every model
	// request, and the ranking carries a wall clock, so the bytes change even
	// when nothing was written. A volatile block sitting ahead of the cache seam
	// rebuilds the cached prefix on every turn, which is measured at a 49.8%
	// token-weighted hit rate against 91.8% on the requests where it holds.
	//
	// The parse default stays "cached" because changing it here would silently
	// move every existing install, and a value in config.json is visible and
	// reversible in a way a compiled default is not. Set memory_delivery to
	// "sidekick" to take the mode whose prompt is provably static.
	MemoryDelivery string `json:"memory_delivery,omitempty"`
	SkillFile      string `json:"skill_file,omitempty"`
	EvaluationFile string `json:"evaluation_file,omitempty"`
	SchedulerFile  string `json:"scheduler_file,omitempty"`
}

// LLMProvider holds LLM API settings.
type LLMProvider struct {
	URL   string `json:"url"`
	Model string `json:"model"`
	// Reasoning selects how much the model thinks before answering: off,
	// minimal, low, medium, high, xhigh, or max. It is advisory; a provider
	// clamps it to what its model supports. Empty leaves the provider on its
	// own default.
	Reasoning string `json:"reasoning,omitempty"`
	// ReasoningEfforts is the effort vocabulary this model declared when it was
	// added, resolved from the model catalog. Empty means unresolved: the level
	// is sent as-is and never clamped.
	ReasoningEfforts []string `json:"reasoning_efforts,omitempty"`
	// ReasoningDefault is the effort the model uses when none is sent, from the
	// same resolution.
	ReasoningDefault string `json:"reasoning_default,omitempty"`
	// ReasoningMandatory is the model always think and cannot be told not to.
	ReasoningMandatory bool   `json:"reasoning_mandatory,omitempty"`
	APIKey             string `json:"api_key,omitempty"`
	ContextLimit       int    `json:"context_limit,omitempty"`
	MaxOutputTokens    int    `json:"max_output_tokens,omitempty"`
	PromptCacheMode    string `json:"prompt_cache_mode,omitempty"`
	PromptCacheTTL     string `json:"prompt_cache_ttl,omitempty"`
	Subscription       string `json:"subscription,omitempty"`
	HermesProvider     string `json:"hermes_provider,omitempty"`
}

type ProviderProfile struct {
	URL       string `json:"url"`
	APIKey    string `json:"api_key,omitempty"`
	Model     string `json:"model"`
	Reasoning string `json:"reasoning,omitempty"`
	// ReasoningEfforts, ReasoningDefault and ReasoningMandatory are the model's
	// thinking parameters as the catalog declared them when the profile was
	// saved. See LLMProvider for what an empty vocabulary means.
	ReasoningEfforts   []string `json:"reasoning_efforts,omitempty"`
	ReasoningDefault   string   `json:"reasoning_default,omitempty"`
	ReasoningMandatory bool     `json:"reasoning_mandatory,omitempty"`
	ContextLimit       int      `json:"context_limit,omitempty"`
	MaxOutputTokens    int      `json:"max_output_tokens,omitempty"`
	PromptCacheMode    string   `json:"prompt_cache_mode,omitempty"`
	PromptCacheTTL     string   `json:"prompt_cache_ttl,omitempty"`
	Subscription       string   `json:"subscription,omitempty"`
	HermesProvider     string   `json:"hermes_provider,omitempty"`
}

// Subscriptions holds provider subscription settings.
type Subscriptions struct {
	Providers map[string]SubProvider `json:"providers"`
}

// SubProvider holds subscription-specific settings.
type SubProvider struct {
	Kind    string            `json:"kind"`
	APIKey  string            `json:"api_key,omitempty"`
	CLIPath string            `json:"cli_path,omitempty"`
	AuthDir string            `json:"auth_dir,omitempty"`
	Extra   map[string]string `json:"extra,omitempty"`
}

type TelegramConfig struct {
	BotToken      string `json:"bot_token,omitempty"`
	AllowedUserID int64  `json:"allowed_user_id,omitempty"`
}

// defaultWorkspaceRoot derives the workspace from the installed binary's
// directory, so an agent's "where my commands run" is a real project directory
// rather than wherever it happened to be started.
//
// A location that cannot survive a restart is not a project. The executable is
// skipped when it lives under the system temp directory, because that is where
// the Go toolchain puts a test binary — /tmp/go-buildNNN/bNNN/pkg.test — and a
// workspace derived from one is a path that no longer exists once the build ends.
// Nothing about that failure is loud: the agent is told its workspace is an empty
// directory, every relative path resolves to nothing, and the only symptom is a
// bot that appears to have forgotten the project it lives in. It reached the live
// config as "/tmp/go-build2223616541/b001/workspace".
func defaultWorkspaceRoot() string {
	if executable, err := os.Executable(); err == nil {
		if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
			executable = resolved
		}
		if !underTempDir(executable) {
			directory := filepath.Dir(executable)
			if filepath.Base(directory) == "bin" {
				directory = filepath.Dir(directory)
			}
			candidate := filepath.Join(directory, "workspace")
			// Only when it can actually be used. A binary installed to
			// /usr/local/bin resolves here to /usr/local/workspace, which a
			// non-root user cannot create, so every relative path the agent
			// resolves would fail against a directory that does not exist. The
			// symptom is the agent reporting files it can plainly see, so the
			// fallback has to be picked before the first command rather than
			// after it goes wrong.
			if creatable(candidate) {
				return candidate
			}
		}
	}
	if workingDirectory, err := os.Getwd(); err == nil && !underTempDir(workingDirectory) {
		return filepath.Join(workingDirectory, "workspace")
	}
	return paths.DefaultWorkspaceRoot()
}

// creatable reports whether a directory could be used at path by this process.
//
// True when it already exists and is writable, or when it does not exist but its
// nearest existing parent is. A path that does not exist yet is the normal case
// for a first run, so treating "missing" as unusable would reject every fresh
// install and send it to the fallback.
func creatable(path string) bool {
	info, err := os.Stat(path)
	switch {
	case err == nil:
		return info.IsDir() && info.Mode().Perm()&0o200 != 0
	case !os.IsNotExist(err):
		return false
	}
	for parent := filepath.Dir(path); ; {
		info, err := os.Stat(parent)
		if err == nil {
			return info.IsDir() && info.Mode().Perm()&0o200 != 0
		}
		next := filepath.Dir(parent)
		if next == parent {
			return false
		}
		parent = next
	}
}

// underTempDir reports whether a path is inside the system temp directory.
//
// Compared on cleaned absolute paths so a symlinked /tmp, the macOS /var ->
// /private/var indirection, or a trailing separator cannot slip past. A name test
// would also be wrong here: it is the location that matters, not the spelling.
func underTempDir(path string) bool {
	temp := strings.TrimRight(filepath.Clean(os.TempDir()), string(filepath.Separator))
	if temp == "" {
		return false
	}
	cleaned := filepath.Clean(path)
	if cleaned == temp {
		return true
	}
	return strings.HasPrefix(cleaned, temp+string(filepath.Separator))
}

// DefaultConfig returns the base defaults before env/file overrides.
func DefaultConfig() *Config {
	base := paths.DataDir()
	return &Config{
		// ContextLimit is deliberately left unset. It used to default to 236000,
		// which made an absent limit indistinguishable from a deliberate one, so the
		// runner could never fall back to the model catalog's advertised window. Zero
		// means "ask the catalog"; the resolver supplies a conservative default when
		// the catalog is silent.
		LLM:                      LLMProvider{MaxOutputTokens: 8192, PromptCacheMode: "auto"},
		ProviderProfiles:         map[string]ProviderProfile{},
		SessionFile:              "sessions",
		WorkspaceRoot:            defaultWorkspaceRoot(),
		MaxToolResultChars:       12000,
		MaxConcurrentTools:       8,
		ModelCatalogURL:          "https://openrouter.ai/api/v1/models",
		ModelCatalogRefreshHours: 24,
		TitleAPIURL:              "https://generativelanguage.googleapis.com/v1beta",
		TitleModel:               "gemini-3.5-flash-lite",
		MemoryFile:               filepath.Join(base, "memory"),
		SkillFile:                filepath.Join(base, "skills"),
		EvaluationFile:           filepath.Join(base, "evaluations.json"),
		SchedulerFile:            filepath.Join(base, "scheduler.json"),
	}
}

// WorkspaceRootEnv is the environment override for the agent's workspace root.
//
// It takes precedence over the config file, the same way CERAA_SYSTEM_PROMPT
// takes precedence over the built-in prompt path: an environment variable is a
// deliberate setting for one invocation, a config file is the standing default.
// The installer writes this into the config rather than exporting it, so this is
// here for the cases an installer cannot cover, such as a container image or a
// CI job whose workspace has to be a mounted volume.
const WorkspaceRootEnv = "CERAA_WORKSPACE_ROOT"

func (c *Config) WorkspaceRootPath() string {
	abs, err := filepath.Abs(resolveWorkspaceRoot(c.WorkspaceRoot))
	if err != nil {
		return resolveWorkspaceRoot(c.WorkspaceRoot)
	}
	return abs
}

// resolveWorkspaceRoot applies the override, kept separate so the precedence is
// stated once and testable without touching the filesystem.
func resolveWorkspaceRoot(configured string) string {
	if override := strings.TrimSpace(os.Getenv(WorkspaceRootEnv)); override != "" {
		return override
	}
	if configured != "" {
		return configured
	}
	return defaultWorkspaceRoot()
}

// mergeProfile overlays a profile onto a base provider, field by field.
//
// Field-by-field rather than wholesale so an unset field in the profile keeps the
// base value. The runtime state and config.json are written by different code and
// do not populate the same fields.
func mergeProfile(base LLMProvider, profile ProviderProfile) LLMProvider {
	if profile.URL != "" {
		base.URL = profile.URL
	}
	if profile.APIKey != "" {
		base.APIKey = profile.APIKey
	}
	if profile.Model != "" {
		base.Model = profile.Model
	}
	if profile.Reasoning != "" {
		base.Reasoning = profile.Reasoning
	}
	if profile.ContextLimit > 0 {
		base.ContextLimit = profile.ContextLimit
	}
	if profile.MaxOutputTokens > 0 {
		base.MaxOutputTokens = profile.MaxOutputTokens
	}
	if profile.PromptCacheMode != "" {
		base.PromptCacheMode = profile.PromptCacheMode
	}
	if profile.PromptCacheTTL != "" {
		base.PromptCacheTTL = profile.PromptCacheTTL
	}
	if profile.Subscription != "" {
		base.Subscription = profile.Subscription
	}
	if profile.HermesProvider != "" {
		base.HermesProvider = profile.HermesProvider
	}
	return base
}

// EffectiveProviderID names the provider that will actually serve, whether it came
// from config.json or from the runtime selection.
//
// ActiveProvider is empty when the active profile was chosen from the Telegram
// menu, so reporting it directly printed "provider=" with nothing after it on a
// perfectly working install.
func (c *Config) EffectiveProviderID() string {
	if id := strings.TrimSpace(c.ActiveProvider); id != "" {
		return id
	}
	if state, _, ok := activeRuntimeSelection(); ok {
		if name := strings.TrimSpace(state.Provider); name != "" {
			return name
		}
	}
	if strings.TrimSpace(c.LLM.URL) == "" && strings.TrimSpace(c.LLM.Model) == "" {
		return "default"
	}
	return "config"
}

// RemoveProviderProfile deletes a stored profile from config.json.
//
// Needed because the menus read both config.json and bridge-state.json. Removal
// that only cleared the runtime store left a config.json copy in place, so the
// provider was still listed immediately after being removed.
func (c *Config) RemoveProviderProfile(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("provider name is empty")
	}
	if id == "default" {
		return fmt.Errorf("the default provider is the base configuration and cannot be removed")
	}
	if _, ok := c.ProviderProfiles[id]; !ok {
		return fmt.Errorf("no provider named %s", id)
	}
	delete(c.ProviderProfiles, id)
	if strings.TrimSpace(c.ActiveProvider) == id {
		c.ActiveProvider = ""
	}
	return Save(c)
}

// ActiveLLM resolves the provider to serve with.
//
// Precedence is deliberate. A provider written in config.json wins, because it is
// something the user typed and can see. Only when config.json names no endpoint
// does the runtime selection apply, so a provider activated from the Telegram menu
// is honoured by every entry point rather than only the one that wrote it. That
// ordering also means adding a config.json provider later takes effect immediately,
// without having to remember to clear the runtime selection.
func (c *Config) ActiveLLM() LLMProvider {
	id := c.ActiveProvider
	if id == "" {
		id = "default"
	}
	profile, ok := c.ProviderProfile(id)
	if !ok {
		return c.withRuntimeSelection(c.LLM)
	}
	return c.withRuntimeSelection(mergeProfile(c.LLM, profile))
}

// withRuntimeSelection adopts the provider the running service selected, but only
// when nothing else named an endpoint.
//
// The trigger is an unresolved endpoint rather than a missing profile, because
// ProviderProfile synthesises a profile from the base block for the "default" id
// and so reports success even when that block is empty. Keying the fallback on
// !ok left it unreachable on the common path, which is the shape of bug this whole
// change exists to fix.
func (c *Config) withRuntimeSelection(base LLMProvider) LLMProvider {
	if strings.TrimSpace(base.URL) != "" {
		return base
	}
	state, profile, found := activeRuntimeSelection()
	if !found || strings.TrimSpace(profile.URL) == "" {
		return base
	}
	merged := mergeProfile(base, profile)
	// Reasoning lives beside the profile in the runtime state rather than inside
	// it, and is a session setting the menu maintains, so it has to be picked up
	// separately or the CLI silently runs at the provider default while the daemon
	// runs at the level the user chose.
	if strings.TrimSpace(merged.Reasoning) == "" {
		merged.Reasoning = state.Reasoning
	}
	return merged
}

func (c *Config) ProviderProfile(id string) (ProviderProfile, bool) {
	if profile, ok := c.ProviderProfiles[id]; ok {
		return profile, true
	}
	if id == "default" {
		return ProviderProfile{URL: c.LLM.URL, APIKey: c.LLM.APIKey, Model: c.LLM.Model, ContextLimit: c.LLM.ContextLimit, MaxOutputTokens: c.LLM.MaxOutputTokens, PromptCacheMode: c.LLM.PromptCacheMode, PromptCacheTTL: c.LLM.PromptCacheTTL, Subscription: c.LLM.Subscription, HermesProvider: c.LLM.HermesProvider}, true
	}
	return ProviderProfile{}, false
}

func (c *Config) ProviderIDs() []string {
	ids := make([]string, 0, len(c.ProviderProfiles)+1)
	ids = append(ids, "default")
	for id := range c.ProviderProfiles {
		if id != "default" {
			ids = append(ids, id)
		}
	}
	return ids
}

// Load loads config from config file + env override + defaults.
// LoadPersisted reads the configuration as stored, without environment overrides.
//
// Load is right for running and wrong for editing. It applies CERAA_* on top of
// the file, so a command that loads and then saves turns a runtime override into
// permanent configuration: setup would write the environment's provider, model
// and token limits into config.json, and unsetting the variable afterwards would
// change nothing. This is the base an editing command should save from.
func LoadPersisted() (*Config, error) {
	cfg := DefaultConfig()
	if err := loadFile(cfg); err != nil {
		return nil, fmt.Errorf("config file: %w", err)
	}
	return cfg, nil
}

func Load() (*Config, error) {
	cfg := DefaultConfig()

	if err := loadFile(cfg); err != nil {
		return nil, fmt.Errorf("config file: %w", err)
	}
	applyEnv(cfg)
	return cfg, nil
}

// applyEnv overlays CERAA_* onto a loaded config.
//
// Split out of Load so the process-wide Load and the Store's Update apply
// exactly the same rules. Two copies of an override list is how a variable
// ends up honoured on one path and ignored on the other.
func applyEnv(cfg *Config) {
	if v := env("PROVIDER"); v != "" {
		cfg.ActiveProvider = v
	}
	if v := env("API_URL"); v != "" {
		cfg.LLM.URL = v
	}
	if v := env("MODEL"); v != "" {
		cfg.LLM.Model = v
	}
	if v := env("API_KEY"); v != "" {
		cfg.LLM.APIKey = v
	}
	// Reasoning sits beside the model rather than inside the LLM block, so it is
	// overridden here rather than in the block above. Every other model-facing
	// setting had an env override and this one did not, which meant the effort the
	// banner reports could only be changed by editing the file.
	if v := env("REASONING"); v != "" {
		cfg.Reasoning = v
	}
	if v := env("CONTEXT_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.LLM.ContextLimit = n
		}
	}
	if v := env("MAX_OUTPUT_TOKENS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.LLM.MaxOutputTokens = n
		}
	}
	if v := env("PROMPT_CACHE_MODE"); v != "" {
		cfg.LLM.PromptCacheMode = v
	}
	if v := env("PROMPT_CACHE_TTL"); v != "" {
		cfg.LLM.PromptCacheTTL = v
	}
	if v := env("SUBSCRIPTION"); v != "" {
		cfg.LLM.Subscription = v
	}
	if v := env("HERMES_PROVIDER"); v != "" {
		cfg.LLM.HermesProvider = v
	}
	if v := env("SESSION_FILE"); v != "" {
		cfg.SessionFile = v
	}
	if v := env("WORKSPACE_ROOT"); v != "" {
		cfg.WorkspaceRoot = v
	}
	if v := env("TOOL_OUTPUT_DIR"); v != "" {
		cfg.ToolOutputDir = v
	}
	if v := env("MAX_TOOL_RESULT_CHARS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxToolResultChars = n
		}
	}
	if v := env("MAX_CONCURRENT_TOOLS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxConcurrentTools = n
		}
	}
	if v := env("MODEL_CATALOG_URL"); v != "" {
		cfg.ModelCatalogURL = v
	}
	if v := env("MODEL_CATALOG_CACHE"); v != "" {
		cfg.ModelCatalogCache = v
	}
	if v := env("MODEL_CATALOG_REFRESH_HOURS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.ModelCatalogRefreshHours = n
		}
	}
	if v := env("TITLE_API_URL"); v != "" {
		cfg.TitleAPIURL = v
	}
	if v := env("TITLE_MODEL"); v != "" {
		cfg.TitleModel = v
	}
	if v := env("MEMORY_FILE"); v != "" {
		cfg.MemoryFile = v
	}
	if v := env("MEMORY_DELIVERY"); v != "" {
		cfg.MemoryDelivery = v
	}
	if v := env("SKILL_FILE"); v != "" {
		cfg.SkillFile = v
	}
	if v := env("EVALUATION_FILE"); v != "" {
		cfg.EvaluationFile = v
	}
	if v := env("SCHEDULER_FILE"); v != "" {
		cfg.SchedulerFile = v
	}
}

// loadedFrom records which file the last loadFile actually read, or empty when it
// read none. A config assembled from a legacy location is working, and the only way
// to know that is to be told, so the path is surfaced rather than inferred.
var loadedFrom string

// Selection is the model-facing choice, read as one value.
//
// It exists so a reader cannot pick the model from one file and the provider from
// another. The menus used to do exactly that: provider and profiles came from
// bridge-state.json while the CLI read config.json, so a screen could describe a
// provider the run path was not using.
type Selection struct {
	ActiveProvider string `json:"active_provider,omitempty"`
	Model          string `json:"model,omitempty"`
	Agent          string `json:"agent,omitempty"`
	Reasoning      string `json:"reasoning,omitempty"`
	// AutoCompaction is a pointer because unset and off are different answers:
	// unset means the user never chose, and the menu defaults that to on.
	AutoCompaction *bool                      `json:"auto_compaction,omitempty"`
	Profiles       map[string]ProviderProfile `json:"provider_profiles,omitempty"`
}

// LoadSelection reads the model-facing selection from the one config file.
func LoadSelection() (Selection, error) {
	cfg, err := Load()
	if err != nil {
		return Selection{}, err
	}
	return Selection{
		ActiveProvider: strings.TrimSpace(cfg.ActiveProvider),
		Model:          strings.TrimSpace(cfg.Model),
		Agent:          strings.TrimSpace(cfg.Agent),
		Reasoning:      strings.TrimSpace(cfg.Reasoning),
		AutoCompaction: cfg.AutoCompaction,
		Profiles:       cfg.ProviderProfiles,
	}, nil
}

// LoadedFrom is the path the most recent Load read, empty if it read no file.
func LoadedFrom() string { return loadedFrom }

// loadFile reads the canonical config.json, falling back to the pre-consolidation
// locations so an existing install is not orphaned by the path change.
//
// The canonical path is tried first and alone decides the result when it exists:
// consulting the fallbacks first would let a stray ./ceraa.json in a working
// directory keep shadowing the real one indefinitely.
func loadFile(cfg *Config) error {
	loadedFrom = ""
	for _, path := range paths.ConfigCandidates() {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := json.Unmarshal(data, cfg); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		loadedFrom = path
		return nil
	}
	return nil
}

func Save(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	path := filepath.Join(paths.DataDir(), "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create config temp file: %w", err)
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
		return fmt.Errorf("set config permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	remove = false
	return nil
}

// candidatePaths lists config.json locations in precedence order. The legacy
// locations are appended so an install that has not been moved still finds its
// config, and a Ceraa config always takes precedence over a pre-rename one.
func candidatePaths() []string {
	return paths.ConfigCandidates()
}

// RuntimeState is the provider selection the running service persists.
//
// DEPRECATED: the provider selection moved into Config itself, so that config has
// exactly one source of truth. This type and the reader below exist only to fold
// an old bridge-state.json into the consolidated config, and are removed once no
// install still needs the migration.
//
// It used to live in bridge-state.json beside chat, project and permission state,
// which is what let the menu and the run path disagree about which provider was
// active: they read the same fact through two different files with two different
// precedence rules. A provider activated from Telegram was therefore invisible to
// the CLI, which reported no endpoint configured for a provider that was working a
// moment earlier.
type RuntimeState struct {
	Provider         string                     `json:"provider,omitempty"`
	Model            string                     `json:"model,omitempty"`
	Reasoning        string                     `json:"reasoning,omitempty"`
	ProviderProfiles map[string]ProviderProfile `json:"provider_profiles,omitempty"`
}

// runtimeStateFile is the on-disk name of the runtime selection.
const runtimeStateFile = "bridge-state.json"

// LoadRuntimeState reads the provider selection the service persisted.
//
// A missing or unreadable file is not an error: a fresh install has no service
// state yet, and that is a normal condition rather than a failure.
func LoadRuntimeState() (RuntimeState, error) {
	var state RuntimeState
	raw, err := os.ReadFile(filepath.Join(paths.DataDir(), runtimeStateFile))
	if err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return state, err
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return RuntimeState{}, fmt.Errorf("parse %s: %w", runtimeStateFile, err)
	}
	return state, nil
}

// activeRuntimeSelection returns the provider the service is currently serving,
// along with the state it came from.
func activeRuntimeSelection() (RuntimeState, ProviderProfile, bool) {
	state, err := LoadRuntimeState()
	if err != nil || len(state.ProviderProfiles) == 0 {
		return RuntimeState{}, ProviderProfile{}, false
	}
	id := state.Provider
	if id == "" {
		id = "default"
	}
	profile, ok := state.ProviderProfiles[id]
	if !ok {
		return RuntimeState{}, ProviderProfile{}, false
	}
	return state, profile, true
}
