package telegram

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"sooraa/harness/sessionnamer"
	"sooraa/harness/sessionstore"
	"sooraa/internal/secret"
)

var titleJobs sync.Map
var pendingSessionAction sync.Map

type sessionAction struct {
	ID     string
	Action string
}

func scheduleSessionTitle(sessionID string) {
	if sessionID == "" || sessionID == "inbox" {
		return
	}
	if _, loaded := titleJobs.LoadOrStore(sessionID, true); loaded {
		return
	}
	go func() {
		defer titleJobs.Delete(sessionID)
		_, _ = generateSessionTitle(sessionID, false)
	}()
}

func generateSessionTitle(sessionID string, force bool) (string, error) {
	store, err := configuredStore()
	if err != nil {
		return "", err
	}
	sess, err := store.Load(sessionID)
	if err != nil {
		return "", err
	}
	if !sessionnamer.HasMeaningfulPrompt(sess) && !force {
		return sess.DisplayTitle(), nil
	}
	if !force && sess.Title != "" {
		if sess.TitleSource != "fallback" || sess.TitleAttempts >= 3 {
			return sess.Title, nil
		}
		if !sess.TitleLastAttempt.IsZero() && time.Since(sess.TitleLastAttempt) < 10*time.Minute {
			return sess.Title, nil
		}
	}
	cfg := sessionnamer.DefaultConfig()
	if cfg.APIKey == "" {
		for _, name := range []string{"gemini_title", "gemini"} {
			if value, secretErr := secret.Get(name); secretErr == nil && value != "" {
				cfg.APIKey = value
				break
			}
		}
	}
	title, generateErr := sessionnamer.Generate(context.Background(), cfg, sess)
	source := "gemini"
	if generateErr != nil {
		if !force {
			title = sessionnamer.Fallback(sess)
			source = "fallback"
		} else {
			return "", generateErr
		}
	}
	if title == "" {
		return "", fmt.Errorf("session has no usable title")
	}
	err = store.Update(sessionID, func(current *sessionstore.Session) error {
		if len(current.Messages) == 0 {
			return fmt.Errorf("session no longer exists")
		}
		if current.Title != "" && !force && (current.TitleSource != "fallback" || current.TitleAttempts >= 3) {
			return nil
		}
		current.Title = title
		current.TitleSource = source
		current.TitleGeneratedAt = time.Now()
		current.TitleLastAttempt = time.Now()
		current.TitleAttempts++
		return nil
	})
	if err != nil {
		return "", err
	}
	return title, nil
}

func renameSession(sessionID, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return fmt.Errorf("title cannot be empty")
	}
	if len([]rune(title)) > 56 {
		return fmt.Errorf("title must be 56 characters or fewer")
	}
	store, err := configuredStore()
	if err != nil {
		return err
	}
	return store.Update(sessionID, func(sess *sessionstore.Session) error {
		sess.Title = title
		sess.TitleSource = "manual"
		sess.TitleGeneratedAt = time.Now()
		sess.TitleLastAttempt = time.Now()
		sess.TitleAttempts = 0
		return nil
	})
}

func deleteSession(sessionID string) error {
	store, err := configuredStore()
	if err != nil {
		return err
	}
	return store.Delete(sessionID)
}
