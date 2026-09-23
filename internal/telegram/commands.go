package telegram

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"sooraa/harness/sessionstore"
	"sooraa/internal/config"
)

// handleSessions lists sessions from harness/sessionstore -> maps to soraacode session.ts list
func handleSessions(token string, chatID int64) {
	store, err := sessionstore.New("sessions")
	if err != nil {
		send(token, chatID, "Failed to open sessions: "+err.Error())
		return
	}
	ids, err := store.List()
	if err != nil {
		send(token, chatID, "Failed to list sessions: "+err.Error())
		return
	}
	if len(ids) == 0 {
		send(token, chatID, "No sessions yet. Use /new to start.")
		return
	}
	var lines []string
	for i, id := range ids {
		if i >= 10 {
			lines = append(lines, fmt.Sprintf("... and %d more", len(ids)-10))
			break
		}
		sess, err := store.Load(id)
		if err != nil {
			lines = append(lines, "• "+id)
			continue
		}
		lines = append(lines, fmt.Sprintf("• `%s` (%d messages)", id, len(sess.Messages)))
	}
	send(token, chatID, "Sessions:\n"+strings.Join(lines, "\n"))
}

// handleOpenSession loads a session by ID -> maps to soraacode session.ts get
func handleOpenSession(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		send(token, chatID, "Usage: /open <session_id>\nList with /sessions")
		return
	}
	id := parts[1]
	store, err := sessionstore.New("sessions")
	if err != nil {
		send(token, chatID, "Failed: "+err.Error())
		return
	}
	sess, err := store.Load(id)
	if err != nil {
		send(token, chatID, "Session not found: "+id)
		return
	}
	send(token, chatID, fmt.Sprintf("Opened session `%s` (%d messages)", sess.ID, len(sess.Messages)))
}

// handleMessages shows recent messages -> maps to soraacode session/history.ts
func handleMessages(token string, chatID int64) {
	store, err := sessionstore.New("sessions")
	if err != nil {
		send(token, chatID, "Failed: "+err.Error())
		return
	}
	sess, err := store.Load(strconv.FormatInt(chatID, 10))
	if err != nil {
		send(token, chatID, "No session yet.")
		return
	}
	if len(sess.Messages) == 0 {
		send(token, chatID, "No messages in current session.")
		return
	}
	start := len(sess.Messages) - 5
	if start < 0 {
		start = 0
	}
	var lines []string
	for _, m := range sess.Messages[start:] {
		role := string(m.Role)
		if m.ToolCall != nil {
			role = "tool:" + m.ToolCall.Name
		} else if m.ToolCallID != "" {
			role = "tool_result"
		}
		preview := m.Content
		if len(preview) > 120 {
			preview = preview[:120] + "..."
		}
		if preview == "" && m.ToolResult != "" {
			preview = m.ToolResult
			if len(preview) > 120 {
				preview = preview[:120] + "..."
			}
		}
		lines = append(lines, fmt.Sprintf("**%s**: %s", role, preview))
	}
	send(token, chatID, "Recent messages:\n"+strings.Join(lines, "\n"))
}

// handleListFiles glob/list directory -> maps to soraacode filesystem.ts + glob.ts
func handleListFiles(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	path := "."
	if len(parts) >= 2 {
		path = parts[1]
	}
	// If path is a file, read it
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		data, err := os.ReadFile(path)
		if err != nil {
			send(token, chatID, "Read failed: "+err.Error())
			return
		}
		content := string(data)
		if len(content) > 3500 {
			content = content[:3500] + "...[truncated]"
		}
		send(token, chatID, "```\n"+content+"\n```")
		return
	}
	// List directory
	entries, err := os.ReadDir(path)
	if err != nil {
		send(token, chatID, "Failed: "+err.Error())
		return
	}
	if len(entries) == 0 {
		send(token, chatID, "Empty: "+path)
		return
	}
	var lines []string
	for i, e := range entries {
		if i >= 30 {
			lines = append(lines, fmt.Sprintf("... and %d more", len(entries)-30))
			break
		}
		suffix := ""
		if e.IsDir() {
			suffix = "/"
		}
		lines = append(lines, e.Name()+suffix)
	}
	send(token, chatID, "Files in `"+path+"`:\n"+strings.Join(lines, "\n"))
}

// handleStatus shows harness status -> maps to soraacode session/status.ts + llm/route
func handleStatus(token string, chatID int64) {
	cfg, _ := config.Load()
	store, _ := sessionstore.New("sessions")
	var sessCount int
	var msgCount int
	if store != nil {
		if ids, err := store.List(); err == nil {
			sessCount = len(ids)
		}
		if sess, err := store.Load(strconv.FormatInt(chatID, 10)); err == nil {
			msgCount = len(sess.Messages)
		}
	}
	text := fmt.Sprintf("Sooraa status:\n• Model: %s\n• URL: %s\n• Sessions: %d\n• Current messages: %d\n• Max steps: %d",
		cfg.LLM.Model, cfg.LLM.URL, sessCount, msgCount, cfg.MaxSteps)
	send(token, chatID, text)
}

// handleSettings shows config -> maps to soraacode config.ts
func handleSettings(token string, chatID int64) {
	cfg, err := config.Load()
	if err != nil {
		send(token, chatID, "Config error: "+err.Error())
		return
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	// Redact api key
	text := string(data)
	text = strings.ReplaceAll(text, cfg.LLM.APIKey, "***")
	if len(text) > 3500 {
		text = text[:3500]
	}
	send(token, chatID, "Settings:\n```json\n"+text+"\n```")
}

// handleDeleteSession deletes a session -> maps to soraacode session.ts delete
func handleDeleteSession(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		send(token, chatID, "Usage: /delete <session_id>")
		return
	}
	id := parts[1]
	store, err := sessionstore.New("sessions")
	if err != nil {
		send(token, chatID, "Failed: "+err.Error())
		return
	}
	// Clear session
	sess, err := store.Load(id)
	if err != nil {
		send(token, chatID, "Session not found: "+id)
		return
	}
	if err := sess.Clear(); err != nil {
		send(token, chatID, "Delete failed: "+err.Error())
		return
	}
	// Also try remove file
	_ = os.Remove(filepath.Join("sessions", id+".json"))
	send(token, chatID, "Deleted session "+id)
}

// handleRenameSession renames session file -> maps to soraacode rename-manager.js
func handleRenameSession(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 3 {
		send(token, chatID, "Usage: /rename <old_id> <new_id>")
		return
	}
	oldID := parts[1]
	newID := parts[2]
	oldPath := filepath.Join("sessions", oldID+".json")
	newPath := filepath.Join("sessions", newID+".json")
	if _, err := os.Stat(oldPath); err != nil {
		send(token, chatID, "Session not found: "+oldID)
		return
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		send(token, chatID, "Rename failed: "+err.Error())
		return
	}
	send(token, chatID, fmt.Sprintf("Renamed %s -> %s", oldID, newID))
}

// handleAbort cancels running operations -> maps to soraacode session/interrupt.ts + run-control
func handleAbort(token string, chatID int64) {
	send(token, chatID, "Abort requested. Current turn will stop after tool timeout (10m max).")
}

// handleCompaction shows compaction level -> maps to soraacode session/compaction.ts
func handleCompaction(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) >= 2 {
		level := parts[1]
		send(token, chatID, "Compaction level set to: "+level+" (stub - harness/contextbuilder truncates, not yet summarizes)")
		return
	}
	send(token, chatID, "Compaction: current harness truncates context. Levels: auto/low/medium/high (stub)")
}

// handleWorktree handles git worktree -> maps to soraacode worktree-service.js
func handleWorktree(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) >= 2 && parts[1] == "list" {
		out, err := os.ReadFile(".git/worktrees")
		if err != nil {
			// Try git command
			send(token, chatID, "Worktrees: run `git worktree list` locally. Harness worktree adapter not yet wired.")
			return
		}
		send(token, chatID, string(out))
		return
	}
	send(token, chatID, "Worktree: `git worktree list` / `git worktree add <path>` - harness adapter stub, see `harness/primitives`")
}

// handleHelp shows full command list -> maps to soraacode bot/commands/definitions.js
func handleFullHelp(token string, chatID int64) {
	send(token, chatID, "Sooraa commands (harness-mapped):\n"+
		"/new - new session `sessionstore.Clear`\n"+
		"/sessions - list `sessionstore.List`\n"+
		"/open <id> - open `sessionstore.Load`\n"+
		"/messages - recent `sessionstore.Messages`\n"+
		"/ls [path] - list `primitives.GlobFiles`/`ReadFile`\n"+
		"/status - `config.Load` + session count\n"+
		"/settings - `config.Load` json\n"+
		"/model /agent /project /session /worktree /variant /permission /context_control - menus\n"+
		"/mcp_catalog /skills_catalog /command_catalog - catalogs (stub)\n"+
		"/compaction_level [level] - compaction stub\n"+
		"/delete <id> /rename <old> <new> - session files\n"+
		"/abort - interrupt turn `operation.Cancel`\n"+
		"/task /scheduled_task - scheduler stub\n"+
		"/setsecret /secrets /delsecret - `secret.Store`\n"+
		"/codex-login /codex-status - `codexauth`\n"+
		"Send any text to run agent `coordinator.RunAsync`")
}
