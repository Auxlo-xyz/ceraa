package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"sooraa/harness/event"
	"sooraa/harness/permission"
	"sooraa/harness/scheduler"
	"sooraa/internal/codexauth"
	"sooraa/internal/config"
	"sooraa/internal/logger"
	"sooraa/internal/secret"
	"sooraa/internal/telegram/keyboard"
	"sooraa/internal/telegram/menus"
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
			MessageID int    `json:"message_id"`
			Text      string `json:"text"`
		} `json:"message"`
	} `json:"callback_query"`
}

var telegramAPIBase = "https://api.telegram.org/bot"
var telegramClient = &http.Client{Timeout: 10 * time.Second}
var telegramPollClient = &http.Client{Timeout: 45 * time.Second}

func api(method, token string) string { return telegramAPIBase + token + "/" + method }

func registerCommands(token string) error {
	body, err := json.Marshal(commandNames())
	if err != nil {
		return err
	}
	resp, err := doTelegramPost(api("setMyCommands", token), body)
	if err != nil {
		return err
	}
	if resp == nil {
		return fmt.Errorf("setMyCommands returned no response")
	}
	defer resp.Body.Close()
	var result struct {
		Ok          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if !result.Ok {
		return fmt.Errorf("setMyCommands: %s", result.Description)
	}
	return nil
}

func applyStateOverrides() {
	model, _, _, steps := defaultState.values()
	if model != "" {
		_ = os.Setenv("SOORAA_MODEL", model)
	}
	if steps > 0 {
		_ = os.Setenv("SOORAA_MAX_STEPS", strconv.Itoa(steps))
	}
	defaultState.mu.RLock()
	alwaysAllow := make(map[string]bool, len(defaultState.AlwaysAllow))
	for toolName, allowed := range defaultState.AlwaysAllow {
		alwaysAllow[toolName] = allowed
	}
	defaultState.mu.RUnlock()
	for toolName, allowed := range alwaysAllow {
		if allowed {
			permission.Default.AllowAlways(toolName)
		}
	}
}

// doTelegramPost posts with rate-limit retry -> maps to soraacode bot/telegram-client-options.js withTelegramRateLimitRetry max 5
func doTelegramPost(url string, body []byte) (*http.Response, error) {
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		resp, err = telegramClient.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var out struct {
				Parameters struct {
					RetryAfter int `json:"retry_after"`
				} `json:"parameters"`
			}
			_ = json.Unmarshal(raw, &out)
			wait := time.Duration(out.Parameters.RetryAfter) * time.Second
			if wait == 0 {
				wait = time.Duration(attempt+1) * 2 * time.Second
			}
			time.Sleep(wait)
			continue
		}
		break
	}
	return resp, err
}

func discardTelegramResponse(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func send(token string, chatID int64, text string) int {
	return sendWithKeyboard(token, chatID, text, keyboard.Main())
}
func sendWithKeyboard(token string, chatID int64, text string, kb map[string]any) int {
	// Rendering pipeline -> maps to soraacode bot/render/limits.js 4096 chunking + markdown fallback
	if len(text) > 4000 {
		// Chunk long messages per soraacode limits.js
		for len(text) > 4000 {
			chunk := text[:4000]
			text = text[4000:]
			sendWithKeyboard(token, chatID, chunk, nil)
		}
	}
	payload := map[string]any{"chat_id": chatID, "text": text, "parse_mode": "Markdown"}
	if kb != nil {
		payload["reply_markup"] = kb
	}
	b, _ := json.Marshal(payload)
	resp, _ := doTelegramPost(api("sendMessage", token), b)
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
	resp, _ := doTelegramPost(api("sendMessage", token), b)
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
	resp, _ := doTelegramPost(api("editMessageText", token), b)
	discardTelegramResponse(resp)
}

// promptQueue serializes prompts per chat -> maps to soraacode app/managers/prompt-queue-manager.js
var promptQueue = struct {
	mu    sync.Mutex
	busy  map[int64]bool
	queue map[int64][]string
}{
	busy:  make(map[int64]bool),
	queue: make(map[int64][]string),
}

var activeRuns sync.Map
var detachedChats sync.Map

func Run() error {
	cfg, cfgErr := config.Load()
	if cfgErr != nil {
		return fmt.Errorf("load config: %w", cfgErr)
	}
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		token = cfg.Telegram.BotToken
	}
	if token == "" {
		return fmt.Errorf("TELEGRAM_BOT_TOKEN required")
	}

	// Initialize logger -> maps to soraacode observability/logging.ts initializeLogger
	if _, err := logger.Initialize(""); err != nil {
		fmt.Printf("logger init failed: %v\n", err)
	} else {
		logger.Info("Sooraa bridge starting")
		defer logger.Get().Flush()
	}

	// Initialize encrypted secret store
	home, _ := os.UserHomeDir()
	keyFile := filepath.Join(home, ".config", "sooraa", "secret.key")
	secretFile := filepath.Join(home, ".config", "sooraa", "secrets.json")
	if err := secret.Init(keyFile, secretFile); err != nil {
		return fmt.Errorf("secret store: %w", err)
	}

	allow := os.Getenv("TELEGRAM_ALLOWED_USER_ID")
	var allowID int64
	if allow == "" {
		allowID = cfg.Telegram.AllowedUserID
	} else {
		allowID, _ = strconv.ParseInt(strings.TrimSpace(allow), 10, 64)
	}
	applyStateOverrides()
	if err := registerCommands(token); err != nil {
		logger.Warn("Telegram command registration failed: " + err.Error())
	} else {
		logger.Info("Telegram command list registered")
	}
	schedulerStore, schedulerErr := scheduler.New(cfg.SchedulerFile)
	if schedulerErr != nil {
		logger.Warn("Scheduler disabled: " + schedulerErr.Error())
	} else {
		_ = schedulerStore.Recover(time.Now())
		schedulerCtx, cancelScheduler := context.WithCancel(context.Background())
		defer cancelScheduler()
		go func() {
			err := schedulerStore.RunWithInterval(schedulerCtx, 30*time.Second, func(runCtx context.Context, job scheduler.Job) error {
				return runAgentWithConfigContextAt(runCtx, job.SessionID, job.Prompt, cfg, job.Project, nil, nil, func(answer string) {
					if job.ChatID != 0 {
						send(token, job.ChatID, "⏰ "+job.Name+": "+answer)
					}
				})
			})
			if err != nil && schedulerCtx.Err() == nil {
				logger.Warn("Scheduler stopped: " + err.Error())
			}
		}()
	}
	offset := getInitialOffset(token)
	fmt.Printf("telegram polling start offset=%d\n", offset)
	for {
		resp, err := telegramPollClient.Get(api(fmt.Sprintf("getUpdates?timeout=30&offset=%d&allowed_updates=%s", offset, "%5B%22message%22%2C%22callback_query%22%5D"), token))
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
			fmt.Printf("update=%d has_callback=%t has_message=%t\n", u.UpdateID, u.CallbackQuery != nil, u.Message != nil)

			// Check local inbox cache to prevent processing duplicate updates
			if u.UpdateID > 0 && GetDefaultInbox().IsDuplicate(u.UpdateID) {
				fmt.Printf("inbox: duplicate update_id %d skipped\n", u.UpdateID)
				continue
			}

			if u.CallbackQuery != nil {
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				chatID := int64(0)
				if u.CallbackQuery.Message != nil {
					chatID = u.CallbackQuery.Message.Chat.ID
				}
				fmt.Printf("callback update=%d data=%q message=%d\n", u.UpdateID, u.CallbackQuery.Data, chatID)
				if !authorized(allowID, u.CallbackQuery.From.ID, chatID) {
					send(token, chatID, "Unauthorized.")
					continue
				}
				handleCallback(token, u.CallbackQuery)
				continue
			}
			if u.Message == nil {
				continue
			}
			if !authorized(allowID, u.Message.From.ID, u.Message.Chat.ID) {
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				send(token, u.Message.Chat.ID, "Unauthorized.")
				continue
			}
			// media handlers -> map to soraacode app/services/file-browser-service + document-extractor + stt-service
			if len(u.Message.Photo) > 0 {
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				// Download largest photo via getFile -> attach to harness
				fileID := u.Message.Photo[len(u.Message.Photo)-1].FileID
				if path, err := downloadTelegramFile(token, fileID); err == nil {
					send(token, u.Message.Chat.ID, "📷 Photo saved: "+path+" — send a prompt describing what to do with it, it will be attached via harness/tool view_image")
				} else {
					send(token, u.Message.Chat.ID, "📷 Photo received: "+telegramFilePreview(fileID)+" (download failed: "+err.Error()+")")
				}
				continue
			}
			if u.Message.Document != nil {
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				if path, err := downloadTelegramFile(token, u.Message.Document.FileID); err == nil {
					send(token, u.Message.Chat.ID, "📄 Document saved: "+path+" ("+u.Message.Document.FileName+") — send a prompt to process it via harness read/tool")
				} else {
					send(token, u.Message.Chat.ID, "📄 Document: "+u.Message.Document.FileName+" (download failed)")
				}
				continue
			}
			if u.Message.Voice != nil {
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				// Voice -> STT stub maps to soraacode stt-service.js whisper-large-v3-turbo
				if path, err := downloadTelegramFile(token, u.Message.Voice.FileID); err == nil {
					send(token, u.Message.Chat.ID, "🎤 Voice saved: "+path+" — STT via whisper not yet wired, send text prompt instead")
				} else {
					send(token, u.Message.Chat.ID, "🎤 Voice received (download failed)")
				}
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
			txt := normalizeCommandText(strings.TrimSpace(u.Message.Text))
			if handlePendingInput(token, u.Message.Chat.ID, txt) {
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				continue
			}
			// Handle secret commands first
			if handleSecretCommands(token, u.UpdateID, u.Message.Chat.ID, txt) {
				continue
			}
			// Handle harness-mapped commands with args
			if handleHarnessCommands(token, u.UpdateID, u.Message.Chat.ID, txt) {
				continue
			}

			switch txt {
			case "/start":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				sendMenuMarkup(token, u.Message.Chat.ID, "Sooraa ready. Choose a control or send a task.", menus.Hub())
			case "/codex-login", "/codex_login":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleCodexLogin(token, u.Message.Chat.ID)
			case "/codex-status", "/codex_status":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleCodexStatus(token, u.Message.Chat.ID)
			case "/help":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleFullHelp(token, u.Message.Chat.ID)
			case "/sessions":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleSessions(token, u.Message.Chat.ID)
			case "/messages":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleMessages(token, u.Message.Chat.ID)
			case "/status":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleStatus(token, u.Message.Chat.ID)
			case "/tools":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleToolsCommand(token, u.Message.Chat.ID)
			case "/todos":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleTodosCommand(token, u.Message.Chat.ID)
			case "/memory":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleMemoryCommand(token, u.Message.Chat.ID, txt)
			case "/skill":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleSkillCommand(token, u.Message.Chat.ID, txt)
			case "/eval":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleEvalCommand(token, u.Message.Chat.ID)
			case "/schedule":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleScheduleCommand(token, u.Message.Chat.ID, txt)
			case "/context", "/context_control":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleContextCommand(token, u.Message.Chat.ID)
			case "/permission":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handlePermissionCommand(token, u.Message.Chat.ID)
			case "/settings":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleSettings(token, u.Message.Chat.ID)
			case "/abort":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleAbort(token, u.Message.Chat.ID)
			case "/detach":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				detachedChats.Store(u.Message.Chat.ID, true)
				send(token, u.Message.Chat.ID, "🔌 Detached. Send /attach to reattach to this session.")
			case "/attach":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				detachedChats.Delete(u.Message.Chat.ID)
				send(token, u.Message.Chat.ID, "🔗 Attached to session "+currentSessionID(u.Message.Chat.ID))
			case "/menu":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				sendMenuMarkup(token, u.Message.Chat.ID, "📋 Sooraa control hub", menus.Hub())
			case "/new":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				startNewSession(token, u.Message.Chat.ID)
			case "/model", "🧠 Model":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleModelCommand(token, u.Message.Chat.ID, "/model")
			case "/agent", "🤖 Agent":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleAgentCommand(token, u.Message.Chat.ID, "/agent")
			case "📋 Menu":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				sendMenuMarkup(token, u.Message.Chat.ID, "📋 Sooraa control hub", menus.Hub())
			case "📊 Context":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleContextCommand(token, u.Message.Chat.ID)
			case "💭 Variant":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleVariantCommand(token, u.Message.Chat.ID, "/variant")
			default:
				if strings.HasPrefix(txt, "/") {
					name := strings.TrimPrefix(strings.Fields(txt)[0], "/")
					for _, m := range menus.All {
						if m == name {
							GetDefaultInbox().CheckAndRecord(u.UpdateID)
							b, _ := json.Marshal(map[string]any{"chat_id": u.Message.Chat.ID, "text": m, "reply_markup": menus.InlineForChat(m, 0, u.Message.Chat.ID, currentSessionID(u.Message.Chat.ID))})
							resp, _ := doTelegramPost(api("sendMessage", token), b)
							discardTelegramResponse(resp)
							goto next
						}
					}
					GetDefaultInbox().CheckAndRecord(u.UpdateID)
					send(token, u.Message.Chat.ID, "Unknown command. Try /menu")
					continue
				next:
				} else {
					if detached, ok := detachedChats.Load(u.Message.Chat.ID); ok && detached == true {
						GetDefaultInbox().CheckAndRecord(u.UpdateID)
						send(token, u.Message.Chat.ID, "🔌 Detached. Send /attach before sending a prompt.")
						continue
					}
					// Deduplicate user prompt request before triggering agent execution
					if !GetDefaultInbox().CheckAndRecord(u.UpdateID) {
						fmt.Printf("inbox: duplicate update_id %d skipped before agent execution\n", u.UpdateID)
						continue
					}
					enqueuePrompt(token, u.Message.Chat.ID, txt, u.UpdateID)
				}
			}
		}
	}
}

func normalizeCommandText(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return text
	}
	command := strings.TrimPrefix(fields[0], "/")
	if at := strings.IndexByte(command, '@'); at >= 0 {
		command = command[:at]
	}
	fields[0] = "/" + command
	return strings.Join(fields, " ")
}

func authorized(allowID, userID, chatID int64) bool {
	return allowID == 0 || userID == allowID || chatID == allowID
}

func handlePrompt(token string, chatID int64, prompt string, updateID int) {
	sessionID := currentSessionID(chatID)
	pinned := getPinnedManager(token, chatID)
	pinned.Init()
	thinkingID := sendThinking(token, chatID, "", false)
	toolID := 0
	var toolLines []string
	var reasoning string
	lastEdit := time.Now()

	// SSE-like event subscription -> maps to soraacode soraacode/events.js subscribeToEvents + event-subscription-service.js
	// Subscribe to harness event bus for this session, with reconnect like soraacode 1s->15s backoff, 30s idle
	eventDone := make(chan struct{})
	eventReady := make(chan struct{})
	go func() {
		backoff := time.Second
		consecutiveTimeouts := 0
		for {
			eventCh := event.Default.Subscribe(sessionID, 64)
			select {
			case <-eventReady:
			default:
				close(eventReady)
			}
			idleTimer := time.NewTimer(30 * time.Second)
			innerDone := false
			for !innerDone {
				select {
				case e, ok := <-eventCh:
					if !ok {
						innerDone = true
						break
					}
					// Reset idle on event
					if !idleTimer.Stop() {
						select {
						case <-idleTimer.C:
						default:
						}
					}
					idleTimer.Reset(30 * time.Second)
					consecutiveTimeouts = 0
					backoff = time.Second
					switch e.Type {
					case event.TypeThinking:
						if full, ok := e.Data["full"].(string); ok && full != "" {
							if time.Since(lastEdit) > 1200*time.Millisecond {
								editThinking(token, chatID, thinkingID, full, false)
								lastEdit = time.Now()
							}
						}
					case event.TypePermission:
						tool, _ := e.Data["tool"].(string)
						reqID, _ := e.Data["request_id"].(string)
						kb := map[string]any{"inline_keyboard": [][]map[string]string{
							{{"text": "✓ Allow once", "callback_data": "perm:allowOnce:" + reqID + ":" + tool}},
							{{"text": "✓ Always", "callback_data": "perm:allowAlways:" + reqID + ":" + tool}},
							{{"text": "✗ Deny", "callback_data": "perm:deny:" + reqID + ":" + tool}},
						}}
						b, _ := json.Marshal(map[string]any{"chat_id": chatID, "text": "🔒 Permission required for `" + tool + "`\n`" + fmt.Sprint(e.Data["args"]) + "`", "parse_mode": "Markdown", "reply_markup": kb})
						resp, _ := doTelegramPost(api("sendMessage", token), b)
						discardTelegramResponse(resp)
					case event.TypeQuestion:
						ques, _ := e.Data["question"].(string)
						if ques == "" {
							ques, _ = e.Data["args"].(string)
							if ques == "" {
								ques = "Question from " + fmt.Sprint(e.Data["tool"])
							}
						}
						reqID, _ := e.Data["request_id"].(string)
						var opts []string
						if v, ok := e.Data["options"]; ok {
							switch arr := v.(type) {
							case []string:
								opts = arr
							case []any:
								for _, o := range arr {
									if s, ok := o.(string); ok {
										opts = append(opts, s)
									}
								}
							}
						}
						if len(opts) == 0 {
							opts = []string{"Yes", "No"}
						}
						var rows [][]map[string]string
						for _, opt := range opts {
							rows = append(rows, []map[string]string{{"text": opt, "callback_data": "qans:" + reqID + ":" + opt}})
						}
						rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
						kb := map[string]any{"inline_keyboard": rows}
						b, _ := json.Marshal(map[string]any{"chat_id": chatID, "text": "❓ " + ques, "reply_markup": kb})
						resp, _ := doTelegramPost(api("sendMessage", token), b)
						discardTelegramResponse(resp)
					case event.TypeCost, event.TypeTokens:
						cost, _ := e.Data["cost"].(string)
						if cost != "" {
							pinned.UpdateCost(cost)
						}
						if tok, ok := e.Data["tokens"].(int); ok {
							pinned.UpdateTokens(tok)
						}
						pinned.UpdateContext("Session: " + sessionID)
					case event.TypeCompact:
						send(token, chatID, "🗜️ Compacted context")
					case event.TypeCompactProgress:
						if prog, ok := e.Data["progress"].(string); ok {
							send(token, chatID, "🗜️ Compact "+prog)
						}
					case event.TypeSubagent:
						if tool, ok := e.Data["tool"].(string); ok {
							send(token, chatID, "🤖 Subagent: "+tool+" running")
						}
					case event.TypeToolStart:
						if tool, ok := e.Data["tool"].(string); ok {
							if elapsed, ok := e.Data["elapsed"].(string); ok {
								send(token, chatID, elapsed)
							} else {
								send(token, chatID, "🔧 Tool start: "+tool)
							}
						}
					case event.TypeToolEnd:
						if out, ok := e.Data["output"].(string); ok {
							if len(out) > 200 {
								out = out[:200] + "..."
							}
							send(token, chatID, "✓ Tool done: "+out)
						}
					}
				case <-idleTimer.C:
					consecutiveTimeouts++
					if consecutiveTimeouts >= 5 {
						logger.Warn(fmt.Sprintf("5 consecutive idle timeouts for session %s", sessionID))
					}
					idleTimer.Reset(30 * time.Second)
				case <-eventDone:
					if !idleTimer.Stop() {
						select {
						case <-idleTimer.C:
						default:
						}
					}
					event.Default.Unsubscribe(sessionID, eventCh)
					return
				}
			}
			// Reconnect with backoff like soraacode events.js
			event.Default.Unsubscribe(sessionID, eventCh)
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			select {
			case <-eventDone:
				return
			default:
			}
			time.Sleep(backoff)
			backoff *= 2
			if backoff > 15*time.Second {
				backoff = 15 * time.Second
			}
		}
	}()
	<-eventReady
	doneCh := make(chan string, 1)
	runCtx, cancelRun := context.WithCancel(context.Background())
	activeRuns.Store(chatID, cancelRun)
	defer activeRuns.Delete(chatID)
	go func() {
		err := runWithThinkingContext(runCtx, sessionID, prompt, func(r string) {
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
			doneCh <- answer
		})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				doneCh <- ""
			} else {
				doneCh <- "error: " + err.Error()
			}
		}
	}()
	select {
	case ans := <-doneCh:
		cancelRun()
		close(eventDone)
		editThinking(token, chatID, thinkingID, reasoning, true)
		if ans != "" {
			// Rendering pipeline chunk via send() already handles 4096 -> soraacode pipeline chunker
			send(token, chatID, ans)
		}
	case <-time.After(10 * time.Minute):
		cancelRun()
		<-doneCh
		close(eventDone)
		send(token, chatID, "Cancelled after timeout.")
	}
	// Drain prompt queue -> soraacode prompt-queue-manager dispatchNextQueuedPrompt
	promptQueue.mu.Lock()
	if len(promptQueue.queue[chatID]) > 0 {
		next := promptQueue.queue[chatID][0]
		promptQueue.queue[chatID] = promptQueue.queue[chatID][1:]
		promptQueue.mu.Unlock()
		go handlePrompt(token, chatID, next, 0)
	} else {
		promptQueue.busy[chatID] = false
		promptQueue.mu.Unlock()
	}
}

func enqueuePrompt(token string, chatID int64, prompt string, updateID int) {
	promptQueue.mu.Lock()
	if promptQueue.busy[chatID] {
		promptQueue.queue[chatID] = append(promptQueue.queue[chatID], prompt)
		n := len(promptQueue.queue[chatID])
		promptQueue.mu.Unlock()
		send(token, chatID, fmt.Sprintf("Queued prompt #%d (busy, will run next)", n))
		return
	}
	promptQueue.busy[chatID] = true
	promptQueue.mu.Unlock()
	go handlePrompt(token, chatID, prompt, updateID)
}

func editTool(token string, chatID int64, msgID int, text string) {
	if len(text) > 3500 {
		text = text[len(text)-3500:]
	}
	b, _ := json.Marshal(map[string]any{"chat_id": chatID, "message_id": msgID, "text": "```\n" + text + "\n```", "parse_mode": "Markdown"})
	resp, _ := doTelegramPost(api("editMessageText", token), b)
	discardTelegramResponse(resp)
}

func getInitialOffset(token string) int {
	resp, err := telegramClient.Get(api("getUpdates?timeout=0&offset=-1&allowed_updates=%5B%22message%22%2C%22callback_query%22%5D", token))
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

func telegramFilePreview(fileID string) string {
	if len(fileID) <= 16 {
		return fileID
	}
	return fileID[:16] + "..."
}

// downloadTelegramFile downloads a Telegram file by file_id -> maps to soraacode app/services/file-download-service.js + document-extractor
func downloadTelegramFile(token, fileID string) (string, error) {
	resp, err := telegramClient.Get(api("getFile?file_id="+fileID, token))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Ok     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	if !out.Ok || out.Result.FilePath == "" {
		return "", fmt.Errorf("getFile failed")
	}
	fileURL := "https://api.telegram.org/file/bot" + token + "/" + out.Result.FilePath
	resp2, err := telegramClient.Get(fileURL)
	if err != nil {
		return "", err
	}
	defer resp2.Body.Close()
	data, _ := io.ReadAll(resp2.Body)
	// Save to /tmp/sooraa-uploads
	dir := filepath.Join(os.TempDir(), "sooraa-uploads")
	_ = os.MkdirAll(dir, 0o755)
	dst := filepath.Join(dir, filepath.Base(out.Result.FilePath))
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return "", err
	}
	return dst, nil
}

func runWithThinking(sessionID string, prompt string, onReasoning func(string), onTool func(string), onAnswer func(string)) error {
	return runAgent(sessionID, prompt, onReasoning, onTool, onAnswer)
}

func runWithThinkingContext(ctx context.Context, sessionID string, prompt string, onReasoning func(string), onTool func(string), onAnswer func(string)) error {
	return runAgentContext(ctx, sessionID, prompt, onReasoning, onTool, onAnswer)
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

// handleHarnessCommands handles harness-mapped commands with args -> maps to soraacode bot/commands
func handleHarnessCommands(token string, updateID int, chatID int64, txt string) bool {
	if txt == "/model" || strings.HasPrefix(txt, "/model ") {
		GetDefaultInbox().CheckAndRecord(updateID)
		handleModelCommand(token, chatID, txt)
		return true
	}
	if txt == "/agent" || strings.HasPrefix(txt, "/agent ") {
		GetDefaultInbox().CheckAndRecord(updateID)
		handleAgentCommand(token, chatID, txt)
		return true
	}
	if txt == "/variant" || strings.HasPrefix(txt, "/variant ") {
		GetDefaultInbox().CheckAndRecord(updateID)
		handleVariantCommand(token, chatID, txt)
		return true
	}
	if txt == "/project" || strings.HasPrefix(txt, "/project ") {
		GetDefaultInbox().CheckAndRecord(updateID)
		handleProjectCommand(token, chatID, txt)
		return true
	}
	if txt == "/steps" || strings.HasPrefix(txt, "/steps ") {
		GetDefaultInbox().CheckAndRecord(updateID)
		handleStepsCommand(token, chatID, txt)
		return true
	}
	if txt == "/search" || strings.HasPrefix(txt, "/search ") {
		GetDefaultInbox().CheckAndRecord(updateID)
		handleSearchCommand(token, chatID, txt)
		return true
	}
	if txt == "/index" || strings.HasPrefix(txt, "/index ") {
		GetDefaultInbox().CheckAndRecord(updateID)
		handleIndexCommand(token, chatID, txt)
		return true
	}
	if txt == "/snapshots" {
		GetDefaultInbox().CheckAndRecord(updateID)
		handleSnapshotsCommand(token, chatID)
		return true
	}
	if txt == "/revert" || strings.HasPrefix(txt, "/revert ") {
		GetDefaultInbox().CheckAndRecord(updateID)
		handleRevertCommand(token, chatID, txt)
		return true
	}
	if txt == "/ls" || strings.HasPrefix(txt, "/ls ") {
		GetDefaultInbox().CheckAndRecord(updateID)
		handleListFiles(token, chatID, txt)
		return true
	}
	if txt == "/open" || strings.HasPrefix(txt, "/open ") {
		GetDefaultInbox().CheckAndRecord(updateID)
		handleOpenSession(token, chatID, txt)
		return true
	}
	if txt == "/delete" || strings.HasPrefix(txt, "/delete ") || strings.HasPrefix(txt, "/delete-session ") {
		GetDefaultInbox().CheckAndRecord(updateID)
		handleDeleteSession(token, chatID, txt)
		return true
	}
	if strings.HasPrefix(txt, "/rename ") {
		GetDefaultInbox().CheckAndRecord(updateID)
		handleRenameSession(token, chatID, txt)
		return true
	}
	if txt == "/worktree" || strings.HasPrefix(txt, "/worktree ") {
		GetDefaultInbox().CheckAndRecord(updateID)
		handleWorktree(token, chatID, txt)
		return true
	}
	if txt == "/compaction_level" || strings.HasPrefix(txt, "/compaction_level ") || txt == "/compaction" || strings.HasPrefix(txt, "/compaction ") {
		GetDefaultInbox().CheckAndRecord(updateID)
		handleCompaction(token, chatID, txt)
		return true
	}
	if txt == "/mcp_catalog" || txt == "/skills_catalog" || txt == "/command_catalog" {
		GetDefaultInbox().CheckAndRecord(updateID)
		handleCatalogCommand(token, chatID, txt)
		return true
	}
	if txt == "/memory" || strings.HasPrefix(txt, "/memory ") {
		GetDefaultInbox().CheckAndRecord(updateID)
		handleMemoryCommand(token, chatID, txt)
		return true
	}
	if txt == "/skill" || strings.HasPrefix(txt, "/skill ") {
		GetDefaultInbox().CheckAndRecord(updateID)
		handleSkillCommand(token, chatID, txt)
		return true
	}
	if txt == "/eval" {
		GetDefaultInbox().CheckAndRecord(updateID)
		handleEvalCommand(token, chatID)
		return true
	}
	if txt == "/schedule" || strings.HasPrefix(txt, "/schedule ") {
		GetDefaultInbox().CheckAndRecord(updateID)
		handleScheduleCommand(token, chatID, txt)
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
	resp, _ := telegramClient.Post(api("sendMessage", token), "application/json", bytes.NewReader(encoded))
	discardTelegramResponse(resp)
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
