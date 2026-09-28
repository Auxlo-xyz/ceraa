package telegram

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"ceraa/harness/llm"
	"ceraa/internal/config"
	"ceraa/internal/envs"
)

// providerState is the Telegram side's single source of truth for which provider,
// model, reasoning level and agent mode are selected.
//
// It used to be a handful of fields on bridgeState, written to bridge-state.json
// beside the chat and project state, while config.json carried a second copy of
// the same facts. Four different readers then applied four different precedence
// rules to those two files, which is how a run could report one provider while the
// menu and the stored state reported another. There is now one file, one struct,
// and one writer.
//
// Reads deliberately go back to the file rather than to a cached copy. A cache is
// what let the in-memory provider disagree with the persisted one in the first
// place, and a second process editing config.json would not be able to invalidate
// it. The file is small and this path is not hot.
type providerState struct {
	store *config.Store
	// writeMu serialises this type's read-modify-write cycles against its own
	// setters. The Store's own lock does not cover the read that precedes an edit,
	// so two menu taps could otherwise both read the old document and the second
	// write would drop the first.
	writeMu sync.Mutex
}

func newProviderState(store *config.Store) *providerState {
	if store == nil {
		store = config.NewStore()
	}
	return &providerState{store: store}
}

// providers is the process-wide instance. Replaced in tests alongside defaultState.
var providers = newProviderState(config.NewStore())

// snapshot reads the current selection.
func (p *providerState) snapshot() (*config.Config, error) { return p.store.Load() }

// active returns the selected provider id, defaulting to "default".
//
// An unreadable config is reported as "default" rather than guessed at: a run on
// the wrong provider is worse than a run that says which provider it could not
// read, and the caller logs the error alongside.
func (p *providerState) active() string {
	cfg, err := p.store.Load()
	if err != nil || strings.TrimSpace(cfg.ActiveProvider) == "" {
		return "default"
	}
	return strings.TrimSpace(cfg.ActiveProvider)
}

func (p *providerState) profiles() map[string]config.ProviderProfile {
	cfg, err := p.store.Load()
	if err != nil {
		return map[string]config.ProviderProfile{}
	}
	out := make(map[string]config.ProviderProfile, len(cfg.ProviderProfiles))
	for key, profile := range cfg.ProviderProfiles {
		out[key] = profile
	}
	return out
}

func (p *providerState) setActive(provider string) error {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "default"
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.store.Update(func(cfg *config.Config) error {
		cfg.ActiveProvider = provider
		return nil
	})
}

func (p *providerState) hasProfile(name string) bool {
	cfg, err := p.store.Load()
	if err != nil {
		return false
	}
	_, ok := cfg.ProviderProfiles[name]
	return ok
}

func (p *providerState) setProfile(name string, profile config.ProviderProfile) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("provider name is empty")
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.store.Update(func(cfg *config.Config) error {
		if cfg.ProviderProfiles == nil {
			cfg.ProviderProfiles = map[string]config.ProviderProfile{}
		}
		cfg.ProviderProfiles[name] = profile
		return nil
	})
}

// removeProfile deletes a stored profile and reports whether one was there.
//
// "default" is refused: it is not a stored profile but the base configuration, so
// deleting it would have nothing to remove and would leave no provider at all.
func (p *providerState) removeProfile(name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, errors.New("provider name is empty")
	}
	if name == "default" {
		return false, errors.New("the default provider is the base configuration and cannot be removed; switch to another provider instead")
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	removed := false
	err := p.store.Update(func(cfg *config.Config) error {
		if _, ok := cfg.ProviderProfiles[name]; !ok {
			return fmt.Errorf("no provider named %s", name)
		}
		delete(cfg.ProviderProfiles, name)
		removed = true
		// Removing the active provider must not leave the selection pointing at a
		// profile that no longer exists.
		if strings.TrimSpace(cfg.ActiveProvider) == name {
			cfg.ActiveProvider = "default"
		}
		return nil
	})
	return removed, err
}

// activeProfile is the profile backing the selected provider.
func (p *providerState) activeProfile() (string, config.ProviderProfile, bool) {
	cfg, err := p.store.Load()
	if err != nil {
		return "default", config.ProviderProfile{}, false
	}
	id := strings.TrimSpace(cfg.ActiveProvider)
	if id == "" {
		id = "default"
	}
	profile, ok := cfg.ProviderProfiles[id]
	if !ok {
		return id, config.ProviderProfile{}, false
	}
	return id, profile, true
}

// modelAndAgent reports the selected model and agent mode.
func (p *providerState) modelAndAgent() (string, string) {
	cfg, err := p.store.Load()
	if err != nil {
		return "", ""
	}
	return strings.TrimSpace(cfg.Model), strings.TrimSpace(cfg.Agent)
}

func (p *providerState) reasoning() string {
	cfg, err := p.store.Load()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(cfg.Reasoning)
}

func (p *providerState) setReasoning(level string) error {
	level = strings.TrimSpace(level)
	if level != "" && !llm.ReasonLevel(level).Valid() {
		return errors.New("unknown reasoning level " + level)
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.store.Update(func(cfg *config.Config) error {
		cfg.Reasoning = level
		return nil
	})
}

func (p *providerState) setModel(model string) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if err := p.store.Update(func(cfg *config.Config) error {
		cfg.Model = strings.TrimSpace(model)
		return nil
	}); err != nil {
		return err
	}
	// The env file is kept in step so a fresh process starts from the same answer.
	return envs.Set("MODEL", model)
}

func (p *providerState) setAgent(agent string) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.store.Update(func(cfg *config.Config) error {
		cfg.Agent = strings.TrimSpace(agent)
		return nil
	})
}

// autoCompaction reports the setting and whether the user has ever set it. The
// second value is what distinguishes "off" from "never chosen", and they mean
// different things to the caller.
func (p *providerState) autoCompaction() (bool, bool) {
	cfg, err := p.store.Load()
	if err != nil || cfg.AutoCompaction == nil {
		return false, false
	}
	return *cfg.AutoCompaction, true
}

func (p *providerState) setAutoCompaction(enabled bool) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.store.Update(func(cfg *config.Config) error {
		cfg.AutoCompaction = &enabled
		return nil
	})
}
