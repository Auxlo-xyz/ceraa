package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"soraa-agent/internal/codexauth"
	"soraa-agent/internal/secret"
	"soraa-agent/internal/telegram/keyboard"
	"soraa-agent/internal/telegram/menus"
)

type update struct {
	UpdateID int `json:"update_id"`
	Message  *struct {
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		From struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Text  string `json:"text"`
		Photo []struct {
			FileID string `json:"file_id"`
		} `json:"photo"`
		Document *struct {
			FileID   string `json:"file_id"`
			FileName string `json:"file_name"`
		} `json:"document"`
		Voice *struct {
			FileID string `json:"file_id"`
		} `json:"voice"`
	} `json:"message"`
	CallbackQuery *struct {
		ID   string `json:"id"`
		From struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Data    string `json:"data"`
		Message *struct {
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
			MessageID int `json:"message_id"`
		} `json:"message"`
	} `json:"callback_query"`
}

func api(method, token string) string { return "https://api.telegram.org/bot" + token + "/" + method }

func send(token string, chatID int64, text string) int {
	return sendWithKeyboard(token, chatID, text, keyboard.Main())
}
func sendWithKeyboard(token string, chatID int64, text string, kb map[string]any) int {
	if len(text) > 4000 {
		text = text[:4000]
	}
	payload := map[string]any{"chat_id": chatID, "text": text, "parse_mode": "Markdown"}
	if kb != nil {
		payload["reply_markup"] = kb
	}
	b, _ := json.Marshal(payload)
	resp, _ := http.Post(api("sendMessage", token), "application/json", bytes.NewReader(b))
	if resp == nil {
		return 0
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var out struct {
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	_ = json.Unmarshal(raw, &out)
	return out.Result.MessageID
}

func sendThinking(token string, chatID int64, reasoning string, collapsed bool) int {
	header := "💭 Thinking..."
	text := header + "\n" + strings.TrimSpace(reasoning)
	if len(text) > 4000 {
		text = text[:4000]
	}
	typ := "blockquote"
	if collapsed {
		typ = "expandable_blockquote"
	}
	entities := []map[string]any{{"type": typ, "offset": len(header) + 1, "length": len(text) - len(header) - 1}}
	b, _ := json.Marshal(map[string]any{"chat_id": chatID, "text": text, "entities": entities})
	resp, _ := http.Post(api("sendMessage", token), "application/json", bytes.NewReader(b))
	if resp == nil {
		return 0
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var out struct {
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	_ = json.Unmarshal(raw, &out)
	return out.Result.MessageID
}

func editThinking(token string, chatID int64, msgID int, reasoning string, collapsed bool) {
	header := "💭 Thinking..."
	text := header + "\n" + strings.TrimSpace(reasoning)
	if strings.TrimSpace(reasoning) == "" {
		text = header
	}
	if len(text) > 4000 {
		text = text[:4000]
	}
	typ := "blockquote"
	if collapsed {
		typ = "expandable_blockquote"
	}
	entities := []map[string]any{{"type": typ, "offset": len(header) + 1, "length": len(text) - len(header) - 1}}
	b, _ := json.Marshal(map[string]any{"chat_id": chatID, "message_id": msgID, "text": text, "entities": entities})
	http.Post(api("editMessageText", token), "application/json", bytes.NewReader(b))
}

func Run() error {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		return fmt.Errorf("TELEGRAM_BOT_TOKEN required")
	}

	// Initialize encrypted secret store
	home, _ := os.UserHomeDir()
	keyFile := filepath.Join(home, ".config", "soraa-agent", "secret.key")
	secretFile := filepath.Join(home, ".config", "soraa-agent", "secrets.json")
	if err := secret.Init(keyFile, secretFile); err != nil {
		return fmt.Errorf("secret store: %w", err)
	}

	allow := os.Getenv("TELEGRAM_ALLOWED_USER_ID")
	var allowID int64
	if allow != "" {
		allowID, _ = strconv.ParseInt(strings.TrimSpace(allow), 10, 64)
	}
	offset := getInitialOffset(token)
	fmt.Printf("telegram polling start offset=%d\n", offset)
	for {
		resp, err := http.Get(api(fmt.Sprintf("getUpdates?timeout=30&offset=%d", offset), token))
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var out struct {
			Ok          bool     `json:"ok"`
			Result      []update `json:"result"`
			ErrorCode   int      `json:"error_code"`
			Description string   `json:"description"`
		}
		_ = json.Unmarshal(raw, &out)
		if out.ErrorCode == 409 {
			fmt.Printf("conflict 409 - backing off 5s: %s\n", out.Description)
			time.Sleep(5 * time.Second)
			continue
		}
		if !out.Ok && out.ErrorCode != 0 {
			fmt.Printf("poll error %d: %s\n", out.ErrorCode, out.Description)
			time.Sleep(2 * time.Second)
			continue
		}
		for _, u := range out.Result {
			offset = u.UpdateID + 1

			// Check local inbox cache to prevent processing duplicate updates
			if u.UpdateID > 0 && GetDefaultInbox().IsDuplicate(u.UpdateID) {
				fmt.Printf("inbox: duplicate update_id %d skipped\n", u.UpdateID)
				continue
			}

			if u.CallbackQuery != nil {
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleCallback(token, u.CallbackQuery)
				continue
			}
			if u.Message == nil {
				continue
			}
			// media handlers
			if len(u.Message.Photo) > 0 {
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				send(token, u.Message.Chat.ID, "📷 Photo received: "+u.Message.Photo[0].FileID[:16]+"...")
				continue
			}
			if u.Message.Document != nil {
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				send(token, u.Message.Chat.ID, "📄 Document: "+u.Message.Document.FileName)
				continue
			}
			if u.Message.Voice != nil {
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				send(token, u.Message.Chat.ID, "🎤 Voice received")
				continue
			}
			if u.Message.Text == "" {
				continue
			}
			if allowID != 0 && u.Message.From.ID != allowID && u.Message.Chat.ID != allowID {
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				send(token, u.Message.Chat.ID, "Unauthorized.")
				continue
			}
			txt := strings.TrimSpace(u.Message.Text)
			// Handle secret commands first
			if handleSecretCommands(token, u.UpdateID, u.Message.Chat.ID, txt) {
				continue
			}

			switch txt {
			case "/start":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				sendWithKeyboard(token, u.Message.Chat.ID, "Soraa Agent ready. Send a task. Use /menu for all 18 menus.", keyboard.Main())
			case "/codex-login":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleCodexLogin(token, u.Message.Chat.ID)
			case "/codex-status":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleCodexStatus(token, u.Message.Chat.ID)
			case "/help":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				send(token, u.Message.Chat.ID, "Commands: /new /menu /model /agent /project /session /settings /setsecret /secrets /delsecret /codex-login /codex-status")
			case "/menu":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				b, _ := json.Marshal(map[string]any{"chat_id": u.Message.Chat.ID, "text": "📋 Hub - 18 menus", "reply_markup": menus.Hub()})
				http.Post(api("sendMessage", token), "application/json", bytes.NewReader(b))
			case "/new":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				clearSession(strconv.FormatInt(u.Message.Chat.ID, 10))
				send(token, u.Message.Chat.ID, "🆕 New session started")
			case "/model", "🧠 Model":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				b, _ := json.Marshal(map[string]any{"chat_id": u.Message.Chat.ID, "text": "Select model", "reply_markup": menus.Inline("model", 0)})
				http.Post(api("sendMessage", token), "application/json", bytes.NewReader(b))
			case "/agent", "🤖 Agent":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				b, _ := json.Marshal(map[string]any{"chat_id": u.Message.Chat.ID, "text": "Select agent", "reply_markup": menus.Inline("agent", 0)})
				http.Post(api("sendMessage", token), "application/json", bytes.NewReader(b))
			case "📊 Context":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				send(token, u.Message.Chat.ID, "Context: 12k/200k 6%")
			case "💭 Variant":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				b, _ := json.Marshal(map[string]any{"chat_id": u.Message.Chat.ID, "text": "Variant", "reply_markup": menus.Inline("variant", 0)})
				http.Post(api("sendMessage", token), "application/json", bytes.NewReader(b))
			default:
				if strings.HasPrefix(txt, "/") {
					name := strings.TrimPrefix(strings.Fields(txt)[0], "/")
					for _, m := range menus.All {
						if m == name {
							GetDefaultInbox().CheckAndRecord(u.UpdateID)
							b, _ := json.Marshal(map[string]any{"chat_id": u.Message.Chat.ID, "text": m, "reply_markup": menus.Inline(m, 0)})
							http.Post(api("sendMessage", token), "application/json", bytes.NewReader(b))
							goto next
						}
					}
					GetDefaultInbox().CheckAndRecord(u.UpdateID)
					send(token, u.Message.Chat.ID, "Unknown command. Try /menu")
					continue
				next:
				} else {
					// Deduplicate user prompt request before triggering agent execution
					if !GetDefaultInbox().CheckAndRecord(u.UpdateID) {
						fmt.Printf("inbox: duplicate update_id %d skipped before agent execution\n", u.UpdateID)
						continue
					}
					go handlePrompt(token, u.Message.Chat.ID, txt, u.UpdateID)
				}
			}
		}
	}
}

func handlePrompt(token string, chatID int64, prompt string, updateID int) {
	sessionID := strconv.FormatInt(chatID, 10)
	thinkingID := sendThinking(token, chatID, "", false)
	toolID := 0
	var toolLines []string
	var reasoning string
	lastEdit := time.Now()
	done := make(chan string, 1)
	go func() {
		err := runWithThinking(sessionID, prompt, func(r string) {
			reasoning = r
			if time.Since(lastEdit) > 1200*time.Millisecond {
				editThinking(token, chatID, thinkingID, reasoning, false)
				lastEdit = time.Now()
			}
		}, func(t string) {
			toolLines = append(toolLines, t)
			text := strings.Join(toolLines, "\n")
			if toolID == 0 {
				toolID = send(token, chatID, "```\n"+text+"\n```")
			} else {
				editTool(token, chatID, toolID, text)
			}
		}, func(answer string) {
			done <- answer
		})
		if err != nil {
			done <- "error: " + err.Error()
		}
	}()
	select {
	case ans := <-done:
		editThinking(token, chatID, thinkingID, reasoning, true)
		if ans != "" {
			send(token, chatID, ans)
		}
	case <-time.After(10 * time.Minute):
		send(token, chatID, "Timeout.")
	}
}

func editTool(token string, chatID int64, msgID int, text string) {
	if len(text) > 3500 {
		text = text[len(text)-3500:]
	}
	b, _ := json.Marshal(map[string]any{"chat_id": chatID, "message_id": msgID, "text": "```\n" + text + "\n```", "parse_mode": "Markdown"})
	http.Post(api("editMessageText", token), "application/json", bytes.NewReader(b))
}

func getInitialOffset(token string) int {
	resp, err := http.Get(api("getUpdates?timeout=0&offset=-1", token))
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Result []update `json:"result"`
	}
	_ = json.Unmarshal(raw, &out)
	if len(out.Result) == 0 {
		return 0
	}
	return out.Result[len(out.Result)-1].UpdateID + 1
}

func runWithThinking(sessionID string, prompt string, onReasoning func(string), onTool func(string), onAnswer func(string)) error {
	return runAgent(sessionID, prompt, onReasoning, onTool, onAnswer)
}

func handleSetSecret(token string, chatID int64, text string) {
	parts := strings.SplitN(text, " ", 3)
	if len(parts) < 3 {
		send(token, chatID, "Usage: /setsecret <name> <value>")
		return
	}
	name := parts[1]
	value := strings.TrimSpace(parts[2])

	if err := secret.Set(name, value); err != nil {
		send(token, chatID, "Failed to store secret: "+err.Error())
		return
	}
	// Never echo the value back
	send(token, chatID, "Secret "+name+" stored securely.")
}

func handleListSecrets(token string, chatID int64) {
	names := secret.List()
	if len(names) == 0 {
		send(token, chatID, "No secrets stored. Use /setsecret <name> <value> to add.")
		return
	}
	var lines []string
	for _, n := range names {
		lines = append(lines, "  /delsecret "+n)
	}
	send(token, chatID, "Stored secrets:\n"+strings.Join(lines, "\n"))
}

func handleDeleteSecret(token string, chatID int64, text string) {
	parts := strings.SplitN(text, " ", 2)
	if len(parts) < 2 {
		send(token, chatID, "Usage: /delsecret <name>")
		return
	}
	name := parts[1]
	if err := secret.Delete(name); err != nil {
		send(token, chatID, "Failed to delete: "+err.Error())
		return
	}
	send(token, chatID, "Secret "+name+" deleted.")
}

// handleSecretCommands returns true if the text was a secret command.
func handleSecretCommands(token string, updateID int, chatID int64, txt string) bool {
	if txt == "/setsecret" {
		GetDefaultInbox().CheckAndRecord(updateID)
		send(token, chatID, "Usage: /setsecret <name> <value>\nExample: /setsecret opencode atr_xxx\nStored encrypted. List with /secrets.")
		return true
	}
	if strings.HasPrefix(txt, "/setsecret ") {
		GetDefaultInbox().CheckAndRecord(updateID)
		handleSetSecret(token, chatID, txt)
		return true
	}
	if txt == "/secrets" {
		GetDefaultInbox().CheckAndRecord(updateID)
		handleListSecrets(token, chatID)
		return true
	}
	if strings.HasPrefix(txt, "/delsecret ") {
		GetDefaultInbox().CheckAndRecord(updateID)
		handleDeleteSecret(token, chatID, txt)
		return true
	}
	return false
}

var authMgr = codexauth.NewManager()

func handleCodexLogin(token string, chatID int64) {
	state, err := authMgr.Start()
	if err != nil {
		send(token, chatID, "Failed to start Codex login: "+err.Error())
		return
	}
	// Send inline keyboard message
	kb, _ := state.InlineKeyboardJSON()
	encoded, _ := json.Marshal(map[string]any{
		"chat_id":      chatID,
		"text":         state.ResultMessage(),
		"reply_markup": json.RawMessage(kb),
	})
	http.Post(api("sendMessage", token), "application/json", bytes.NewReader(encoded))
}

func handleCodexStatus(token string, chatID int64) {
	// Check if already authenticated
	if val, err := secret.Get("codex_authenticated"); err == nil && val == "true" {
		send(token, chatID, "✅ Codex is authenticated. Use /codex-login to re-authenticate.")
		return
	}
	// Check in-flight auth flows
	for _, flow := range authMgr.ListFlows() {
		if flow.Status == "succeeded" {
			send(token, chatID, "✅ Codex login completed! You can now use Codex.")
			return
		}
		if flow.Status == "waiting" || flow.Status == "checking" {
			send(token, chatID, "🔄 Login in progress. User code: "+flow.UserCode)
			return
		}
	}
	send(token, chatID, "❌ Not authenticated. Use /codex-login to start.")
}
