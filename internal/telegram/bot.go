package telegram

import (
	"bytes"
	"ceraa/harness/event"
	"ceraa/harness/llm"
	"ceraa/harness/permission"
	"ceraa/harness/question"
	"ceraa/harness/scheduler"
	"ceraa/internal/codexauth"
	"ceraa/internal/config"
	"ceraa/internal/envs"
	"ceraa/internal/hermesauth"
	"ceraa/internal/logger"
	"ceraa/internal/paths"
	"ceraa/internal/secret"
	"ceraa/internal/telegram/keyboard"
	"ceraa/internal/telegram/menus"
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
		MessageID int    `json:"message_id"`
		Text      string `json:"text"`
		Photo     []struct {
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

func setMyCommandsBody() ([]byte, error) {
	return json.Marshal(map[string]any{"commands": commandNames()})
}

func registerCommands(token string) error {
	body, err := setMyCommandsBody()
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
	model, _, _ := defaultState.values()
	if model != "" {
		_ = envs.Set("MODEL", model)
	}
	if provider := defaultState.provider(); provider != "" {
		_ = envs.Set("PROVIDER", provider)
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
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		resp, err := telegramClient.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			lastErr = err
			if attempt < 4 {
				time.Sleep(time.Duration(attempt+1) * time.Second)
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			raw, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				return nil, fmt.Errorf("telegram HTTP %d: read response: %w", resp.StatusCode, readErr)
			}
			lastErr = fmt.Errorf("telegram HTTP %d: %s", resp.StatusCode, string(raw))
			if resp.StatusCode == 429 || resp.StatusCode >= 500 {
				var out struct {
					Parameters struct {
						RetryAfter int `json:"retry_after"`
					} `json:"parameters"`
				}
				_ = json.Unmarshal(raw, &out)
				if attempt < 4 {
					wait := time.Duration(out.Parameters.RetryAfter) * time.Second
					if wait == 0 {
						wait = time.Duration(attempt+1) * 2 * time.Second
					}
					time.Sleep(wait)
					continue
				}
			}
			return nil, lastErr
		}
		return resp, nil
	}
	return nil, lastErr
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
func sendHub(token string, chatID int64) {
	sendMenuMarkup(token, chatID, "📋 Ceraa hub", menus.Hub())
}

func sendMessagePayload(token string, chatID int64, text string, parseMode string, kb map[string]any) (int, error) {
	payload := map[string]any{"chat_id": chatID, "text": text}
	if parseMode != "" {
		payload["parse_mode"] = parseMode
	}
	if kb != nil {
		payload["reply_markup"] = kb
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	resp, err := doTelegramPost(api("sendMessage", token), body)
	if err != nil {
		return 0, err
	}
	if resp == nil {
		return 0, fmt.Errorf("sendMessage: empty response")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("sendMessage response: %w", err)
	}
	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return 0, fmt.Errorf("sendMessage response: %w", err)
	}
	if !result.OK {
		return 0, fmt.Errorf("sendMessage: %s", result.Description)
	}
	if result.Result.MessageID == 0 {
		return 0, fmt.Errorf("sendMessage: missing message_id")
	}
	return result.Result.MessageID, nil
}

func isTelegramParseError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "parse entities") || strings.Contains(message, "can't parse")
}

func sendWithKeyboard(token string, chatID int64, text string, kb map[string]any) int {
	runes := []rune(text)
	if len(runes) > 4000 {
		for len(runes) > 4000 {
			chunk := string(runes[:4000])
			runes = runes[4000:]
			if sendWithKeyboard(token, chatID, chunk, nil) == 0 {
				return 0
			}
		}
		text = string(runes)
	}
	messageID, err := sendMessagePayload(token, chatID, text, "Markdown", kb)
	if err == nil {
		return messageID
	}
	if isTelegramParseError(err) {
		messageID, fallbackErr := sendMessagePayload(token, chatID, text, "", kb)
		if fallbackErr == nil {
			return messageID
		}
		logger.Warn("Telegram Markdown fallback failed: " + fallbackErr.Error())
		return 0
	}
	logger.Warn("Telegram sendMessage failed: " + err.Error())
	return 0
}

func sendMessageWithMarkup(token string, chatID int64, text string, markup map[string]any) int {
	messageID, err := sendMessagePayload(token, chatID, text, "", markup)
	if err != nil {
		logger.Warn("Telegram sendMessage failed: " + err.Error())
		return 0
	}
	return messageID
}

func sendPlainMessage(token string, chatID int64, text string) int {
	messageID, err := sendMessagePayload(token, chatID, text, "", nil)
	if err != nil {
		logger.Warn("Telegram sendMessage failed: " + err.Error())
		return 0
	}
	return messageID
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

type telegramStartup struct {
	token         string
	providerReady bool
}

func resolveTelegramStartup(cfg *config.Config) (telegramStartup, error) {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		token = cfg.Telegram.BotToken
	}
	if token == "" {
		return telegramStartup{}, fmt.Errorf("TELEGRAM_BOT_TOKEN required")
	}
	return telegramStartup{token: token, providerReady: providerConfigured(cfg)}, nil
}

func Run() error {
	cfg, cfgErr := config.Load()
	if cfgErr != nil {
		return fmt.Errorf("load config: %w", cfgErr)
	}
	if err := os.MkdirAll(cfg.WorkspaceRootPath(), 0o755); err != nil {
		return fmt.Errorf("create workspace root: %w", err)
	}
	startup, err := resolveTelegramStartup(cfg)
	if err != nil {
		return err
	}
	token := startup.token

	// Initialize logger -> maps to soraacode observability/logging.ts initializeLogger
	if _, err := logger.Initialize(""); err != nil {
		fmt.Printf("logger init failed: %v\n", err)
	} else {
		logger.Info("Ceraa bridge starting")
		if !startup.providerReady {
			logger.Info("Telegram token-only startup: provider onboarding required")
		}
		defer logger.Get().Flush()
	}

	// Initialize encrypted secret store
	keyFile := filepath.Join(paths.DataDir(), "secret.key")
	secretFile := filepath.Join(paths.DataDir(), "secrets.json")
	if err := secret.Init(keyFile, secretFile); err != nil {
		return fmt.Errorf("secret store: %w", err)
	}

	if defaultState.loadErr != nil {
		return fmt.Errorf("bridge state: %w", defaultState.loadErr)
	}
	allow, allowSet := os.LookupEnv("TELEGRAM_ALLOWED_USER_ID")
	allowID, err := resolveAllowedUserID(allow, allowSet, cfg.Telegram.AllowedUserID, defaultState.allowedUserID())
	if err != nil {
		return err
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
				return runAgentWithConfigContextAt(runCtx, job.SessionID, job.Prompt, cfg, job.Project, nil, nil, nil, func(answer string) {
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
			logger.Warn("Telegram getUpdates request failed: " + err.Error())
			time.Sleep(2 * time.Second)
			continue
		}
		raw, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			logger.Warn("Telegram getUpdates response read failed: " + readErr.Error())
			time.Sleep(2 * time.Second)
			continue
		}
		var out struct {
			Ok          bool     `json:"ok"`
			Result      []update `json:"result"`
			ErrorCode   int      `json:"error_code"`
			Description string   `json:"description"`
		}
		if unmarshalErr := json.Unmarshal(raw, &out); unmarshalErr != nil {
			logger.Warn("Telegram getUpdates response decode failed: " + unmarshalErr.Error())
			time.Sleep(2 * time.Second)
			continue
		}
		if !out.Ok && out.ErrorCode == 0 {
			logger.Warn("Telegram getUpdates returned an unsuccessful response")
			time.Sleep(2 * time.Second)
			continue
		}
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
				if !authorized(allowID, u.CallbackQuery.From.ID, chatID) && !bootstrapClaimAuthorized(allowID, u.CallbackQuery.From.ID, chatID, u.CallbackQuery.Data) {
					send(token, chatID, "Unauthorized.")
					continue
				}
				handleCallback(token, u.CallbackQuery)
				if configured := defaultState.allowedUserID(); configured > 0 {
					allowID = configured
				}
				continue
			}
			if u.Message == nil {
				continue
			}
			if !authorized(allowID, u.Message.From.ID, u.Message.Chat.ID) && !bootstrapStartAuthorized(allowID, u.Message.From.ID, u.Message.Chat.ID, u.Message.Text) {
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				send(token, u.Message.Chat.ID, "Unauthorized.")
				continue
			}
			// A pending question takes precedence over a new task: the user was
			// asked something, so their next message is the answer.
			if text := strings.TrimSpace(u.Message.Text); text != "" {
				if requestID, waiting := defaultState.takeAnswer(u.Message.Chat.ID); waiting {
					GetDefaultInbox().CheckAndRecord(u.UpdateID)
					if question.Default.Answer(currentSessionID(u.Message.Chat.ID), requestID, text) {
						send(token, u.Message.Chat.ID, "✓ Answered: "+text)
					} else {
						send(token, u.Message.Chat.ID, "That question already expired.")
					}
					continue
				}
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
			txt := normalizeCommandText(strings.TrimSpace(u.Message.Text))
			setMenuTriggerPending(u.Message.Chat.ID, u.Message.MessageID)
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

			if strings.HasPrefix(txt, "/hermes-login") || strings.HasPrefix(txt, "/hermes_login") || strings.HasPrefix(txt, "/hermes-status") || strings.HasPrefix(txt, "/hermes_status") {
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				if strings.HasPrefix(txt, "/hermes-status") || strings.HasPrefix(txt, "/hermes_status") {
					handleHermesStatus(token, u.Message.Chat.ID)
				} else {
					handleHermesLogin(token, u.Message.Chat.ID, txt)
				}
				continue
			}

			switch txt {
			case "/start":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				if !providerConfigured(cfg) {
					sendMenuMarkup(token, u.Message.Chat.ID, "Welcome to Ceraa. Choose a provider to finish onboarding.", menus.Onboarding())
				} else {
					sendHub(token, u.Message.Chat.ID)
				}
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
			case "/hub", "/menu":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				sendHub(token, u.Message.Chat.ID)
			case "/new":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				startNewSession(token, u.Message.Chat.ID)
			case "/model", "🧠 Model":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleModelCommand(token, u.Message.Chat.ID, "/model")
			case "/agent", "🤖 Agent", "🤖 Mode":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleAgentCommand(token, u.Message.Chat.ID, "/agent")
			case "📋 Hub", "📋 Menu":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				sendHub(token, u.Message.Chat.ID)
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
							sendMessageWithMarkup(token, u.Message.Chat.ID, m, menus.InlineForChat(m, 0, u.Message.Chat.ID, currentSessionID(u.Message.Chat.ID)))
							goto next
						}
					}
					GetDefaultInbox().CheckAndRecord(u.UpdateID)
					send(token, u.Message.Chat.ID, "Unknown command. Try /hub")
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

func providerConfigured(cfg *config.Config) bool {
	provider := activeProviderConfig(cfg, defaultState.provider())
	// Subscription providers carry no API key in the profile: their credential
	// comes from the encrypted native store at request time.
	if provider.Subscription == "hermes" {
		return provider.Model != ""
	}
	return provider.URL != "" && provider.Model != "" && provider.APIKey != ""
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

func resolveAllowedUserID(raw string, set bool, configID, stateID int64) (int64, error) {
	if set {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return 0, fmt.Errorf("TELEGRAM_ALLOWED_USER_ID must be a positive integer")
		}
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			return 0, fmt.Errorf("TELEGRAM_ALLOWED_USER_ID must be a positive integer")
		}
		return parsed, nil
	}
	if configID < 0 {
		return 0, fmt.Errorf("Telegram allowed user ID must be positive")
	}
	if configID > 0 {
		return configID, nil
	}
	if stateID < 0 {
		return 0, fmt.Errorf("bridge-state allowed user ID must be positive")
	}
	return stateID, nil
}

func authorized(allowID, userID, chatID int64) bool {
	return allowID > 0 && (userID == allowID || chatID == allowID)
}

func bootstrapStartAuthorized(allowID, userID, chatID int64, text string) bool {
	return allowID == 0 && userID > 0 && userID == chatID && normalizeCommandText(strings.TrimSpace(text)) == "/start"
}

func bootstrapClaimAuthorized(allowID, userID, chatID int64, data string) bool {
	return allowID == 0 && userID > 0 && userID == chatID && data == "onboarding:claim"
}

func handlePrompt(token string, chatID int64, prompt string, updateID int) {
	if defaultState.project(chatID) == "" {
		clearPromptQueue(chatID)
		sendMenuMarkup(token, chatID, "Choose a project before sending a task.", menus.InlineForChat("project", 0, chatID, currentSessionID(chatID)))
		return
	}
	sessionID := currentSessionID(chatID)
	runCtx, cancelRun := context.WithCancel(context.Background())
	activeRuns.Store(chatID, cancelRun)
	defer activeRuns.Delete(chatID)
	renderer := newRunRenderer(token, chatID)
	renderer.setCancel(cancelRun)
	defer renderer.close()

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
					case event.TypePermission:
						tool, _ := e.Data["tool"].(string)
						reqID, _ := e.Data["request_id"].(string)
						kb := map[string]any{"inline_keyboard": [][]map[string]string{
							{{"text": "✓ Allow once", "callback_data": "perm:allowOnce:" + reqID + ":" + tool}},
							{{"text": "✓ Always", "callback_data": "perm:allowAlways:" + reqID + ":" + tool}},
							{{"text": "✗ Deny", "callback_data": "perm:deny:" + reqID + ":" + tool}},
						}}
						sendWithKeyboard(token, chatID, "🔒 Permission required for `"+tool+"`\n"+summariseToolRequest(tool, e.Data["args"]), kb)
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
						freeForm, _ := e.Data["free_form"].(bool)
						var rows [][]map[string]string
						for _, opt := range opts {
							rows = append(rows, []map[string]string{{"text": opt, "callback_data": "qans:" + reqID + ":" + opt}})
						}
						// Without a way to type, the user is limited to the exact
						// strings offered, which is wrong whenever the answer is a
						// value, a path, or a name.
						if freeForm || len(opts) == 0 {
							rows = append(rows, []map[string]string{{"text": "✍️ Type an answer", "callback_data": "qtype:" + reqID}})
						}
						rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
						kb := map[string]any{"inline_keyboard": rows}
						if freeForm {
							// Mark the message as one whose next plain reply is an
							// answer, so the typed text is not mistaken for a task.
							defaultState.expectAnswer(reqID, chatID)
						}
						sendMessageWithMarkup(token, chatID, "❓ "+ques, kb)
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
	// Kept alive for the whole run. A single sendChatAction expires after about
	// five seconds, so a long task would otherwise appear to hang with no
	// feedback. Stopped on every exit path below, including the timeout.
	typing := newTypingIndicator(token, chatID)
	typing.Start()
	defer typing.Stop()
	go func() {
		err := runWithThinkingContext(runCtx, sessionID, prompt, renderer.content, renderer.reasoning, renderer.tool, func(answer string) {
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
		typing.Stop()
		cancelRun()
		close(eventDone)
		if renderer.isStopped() {
			renderer.fail()
		} else if ans != "" {
			renderer.finish(ans)
		} else {
			renderer.fail()
		}
	case <-time.After(10 * time.Minute):
		typing.Stop()
		cancelRun()
		<-doneCh
		close(eventDone)
		renderer.fail()
		send(token, chatID, "Cancelled after timeout.")
	}
	scheduleSessionTitle(sessionID)
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

func clearPromptQueue(chatID int64) {
	promptQueue.mu.Lock()
	delete(promptQueue.queue, chatID)
	promptQueue.busy[chatID] = false
	promptQueue.mu.Unlock()
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
	// Save to /tmp/ceraa-uploads
	dir := filepath.Join(os.TempDir(), "ceraa-uploads")
	_ = os.MkdirAll(dir, 0o755)
	dst := filepath.Join(dir, filepath.Base(out.Result.FilePath))
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return "", err
	}
	return dst, nil
}

func runWithThinking(sessionID string, prompt string, onReasoning func(string), onTool func(string, string), onAnswer func(string)) error {
	return runAgent(sessionID, prompt, onReasoning, onTool, onAnswer)
}

func runWithThinkingContext(ctx context.Context, sessionID string, prompt string, onContent func(string), onReasoning func(string), onTool func(string, string), onAnswer func(string)) error {
	return runAgentContext(ctx, sessionID, prompt, onContent, onReasoning, onTool, onAnswer)
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
var hermesAuthMgr = hermesauth.NewLoginManager(nil)
var hermesLoginModels sync.Map

func handleHermesLogin(token string, chatID int64, text string) {
	parts := strings.Fields(strings.TrimSpace(text))
	model := ""
	if len(parts) > 1 {
		model = parts[1]
		if model == "nous" && len(parts) > 2 {
			model = parts[2]
		}
	}
	flow, err := hermesAuthMgr.Start(context.Background(), "nous")
	if err != nil {
		send(token, chatID, "Failed to start Hermes login: "+err.Error())
		return
	}
	hermesLoginModels.Store(flow.ID, model)
	keyboard, _ := flow.InlineKeyboardJSON()
	var markup map[string]any
	if err := json.Unmarshal(keyboard, &markup); err != nil {
		send(token, chatID, "Failed to encode Hermes login menu: "+err.Error())
		return
	}
	sendMessageWithMarkup(token, chatID, flow.ResultMessage(), markup)
}

func handleHermesStatus(token string, chatID int64) {
	for _, flow := range hermesAuthMgr.ListFlows() {
		status, _, userCode := flow.StatusSnapshot()
		if status == "succeeded" {
			activateHermesProvider(token, chatID, flow.ID)
			return
		}
		if status == "waiting" || status == "checking" {
			send(token, chatID, "🔄 Hermes login in progress. Code: "+userCode)
			return
		}
	}
	send(token, chatID, "❌ No completed Hermes login. Use /hermes-login to start Nous Portal login.")
}

func activateHermesProvider(token string, chatID int64, flowID string) {
	modelValue, _ := hermesLoginModels.Load(flowID)
	model, _ := modelValue.(string)
	if model == "" {
		_, model, _ = defaultState.values()
	}
	if model == "" {
		send(token, chatID, "✅ Hermes login saved. Send /model <model-id> to activate the provider.")
		return
	}
	profile := config.ProviderProfile{Model: model, ContextLimit: 236000, MaxOutputTokens: 8192, PromptCacheMode: "auto", Subscription: "hermes", HermesProvider: "nous"}
	if err := defaultState.setProviderProfile("hermes", profile); err != nil {
		send(token, chatID, "Hermes profile save failed: "+err.Error())
		return
	}
	if err := defaultState.setProvider("hermes"); err != nil {
		send(token, chatID, "Hermes provider activation failed: "+err.Error())
		return
	}
	send(token, chatID, "✅ Hermes/Nous provider activated with model "+model+".")
}

func handleCodexLogin(token string, chatID int64) {
	state, err := authMgr.Start()
	if err != nil {
		send(token, chatID, "Failed to start Codex login: "+err.Error())
		return
	}
	kb, _ := state.InlineKeyboardJSON()
	var markup map[string]any
	if err := json.Unmarshal(kb, &markup); err != nil {
		send(token, chatID, "Failed to encode Codex login menu: "+err.Error())
		return
	}
	sendMessageWithMarkup(token, chatID, state.ResultMessage(), markup)
}

func handleCodexStatus(token string, chatID int64) {
	// A stored token is the real answer, rather than a marker a login happened to
	// leave behind. It is not on its own proof that inference works, so the
	// message says which provider is actually active: a stored credential with
	// another provider selected is not a usable Codex setup.
	if codexauth.Authenticated() {
		active := defaultState.provider()
		_, activeModel, _ := defaultState.values()
		if active == llm.CodexSubscription {
			expiry := "unknown"
			if raw, err := secret.Get("codex_expires_at"); err == nil && raw != "" {
				if seconds, convErr := strconv.ParseInt(raw, 10, 64); convErr == nil {
					expiry = time.Unix(seconds, 0).UTC().Format(time.RFC3339)
				}
			}
			send(token, chatID, "✅ Codex is authenticated and active.\nModel: "+activeModel+"\nAccess token expires: "+expiry+"\nUse /codex-login to sign in again.")
			return
		}
		send(token, chatID, "✅ Codex is authenticated (access token valid until "+codexExpiryLabel()+").\n\nIt is not the active provider — Ceraa is using <b>"+active+"</b>. Send /codex-login or pick 🧠 Codex (ChatGPT) in the provider menu to switch to it.")
		return
	}
	for _, flow := range authMgr.ListFlows() {
		state := flow.Snapshot()
		switch state.Status {
		case codexauth.StatusSucceeded:
			send(token, chatID, "✅ Codex login completed. Codex can now be used.")
			return
		case codexauth.StatusWaiting, codexauth.StatusApproved:
			send(token, chatID, "🔄 Login in progress.\nCode: <code>"+state.UserCode+"</code>")
			return
		}
	}
	send(token, chatID, "❌ Not authenticated. Use /codex-login to start.")
}

// codexExpiryLabel renders the stored expiry for humans, without leaking the
// token itself.
func codexExpiryLabel() string {
	raw, err := secret.Get("codex_expires_at")
	if err != nil || raw == "" {
		return "unknown"
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return "unknown"
	}
	return time.Unix(seconds, 0).UTC().Format(time.RFC3339)
}

// selectCodexProvider handles 🧠 Codex in the provider menu. With a credential
// already stored it activates Codex straight away; otherwise it starts the
// device login and activates as soon as the code is approved.
func selectCodexProvider(token string, chatID int64) {
	if codexauth.Authenticated() {
		activateCodexProvider(token, chatID)
		return
	}
	flow, err := authMgr.Start()
	if err != nil {
		send(token, chatID, "Failed to start Codex login: "+err.Error())
		return
	}
	kb, err := flow.InlineKeyboardJSON()
	if err != nil {
		send(token, chatID, "Failed to encode Codex login menu: "+err.Error())
		return
	}
	var markup map[string]any
	if json.Unmarshal(kb, &markup) != nil {
		send(token, chatID, "Failed to encode Codex login menu.")
		return
	}
	pendingCodexActivation.Store(chatID, true)
	sendMessageWithMarkup(token, chatID,
		"Sign in to ChatGPT to use Codex. Open the link, approve the code, and Ceraa will switch to Codex automatically.\n\n"+flow.ResultMessage(),
		markup)
}

// activateCodexProvider saves the Codex profile and makes it active. The model
// is one this account can actually serve, because the endpoint rejects anything
// else outright.
func activateCodexProvider(token string, chatID int64) {
	model := llm.DefaultCodexModel
	if configured := codexModelForAccount(); configured != "" {
		model = configured
	}
	profile := config.ProviderProfile{
		URL:             llm.CodexBaseURL,
		Model:           model,
		ContextLimit:    codexContextLimit,
		MaxOutputTokens: 8192,
		Subscription:    llm.CodexSubscription,
	}
	if err := defaultState.setProviderProfile(llm.CodexSubscription, profile); err != nil {
		send(token, chatID, "Codex profile save failed: "+err.Error())
		return
	}
	if err := defaultState.setProvider(llm.CodexSubscription); err != nil {
		send(token, chatID, "Codex provider activation failed: "+err.Error())
		return
	}
	pendingCodexActivation.Delete(chatID)
	send(token, chatID, "✅ Codex provider activated with model <code>"+model+"</code>.\nSend a task to use it.")
}

// pendingCodexActivation records the chats that asked for Codex and are waiting
// on a device login, so approval can switch the provider without another tap.
var pendingCodexActivation sync.Map

// codexContextLimit is a conservative local budget. The endpoint does not
// advertise a context size, so this is a safety margin for compaction rather
// than a verified figure.
const codexContextLimit = 200000

// codexModelForAccount returns a configured Codex model when one is already
// known to work, so a re-login keeps the user's choice.
func codexModelForAccount() string {
	_, model, _ := defaultState.values()
	if llm.IsCodexModel(model) {
		return model
	}
	return ""
}
