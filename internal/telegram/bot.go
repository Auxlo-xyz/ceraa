package telegram

import (
	"bytes"
	"ceraa/harness/event"
	"ceraa/harness/llm"
	"ceraa/harness/permission"
	"ceraa/harness/question"
	"ceraa/harness/scheduler"
	"ceraa/harness/tool"
	"ceraa/internal/clineauth"
	"ceraa/internal/codexauth"
	"ceraa/internal/config"
	"ceraa/internal/envs"
	"ceraa/internal/hermesauth"
	"ceraa/internal/logger"
	"ceraa/internal/miniapp"
	"ceraa/internal/minimaxauth"
	"ceraa/internal/oauthdevice"
	"ceraa/internal/opencodeauth"
	"ceraa/internal/paths"
	"ceraa/internal/procfile"
	"ceraa/internal/provision"
	"ceraa/internal/secret"
	"ceraa/internal/telegram/keyboard"
	"ceraa/internal/telegram/menus"
	"ceraa/internal/xaiauth"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	CallbackQuery *callbackQuery `json:"callback_query"`
}

var telegramAPIBase = "https://api.telegram.org/bot"
var telegramClient = &http.Client{Timeout: 10 * time.Second}
var telegramPollClient = &http.Client{Timeout: 45 * time.Second}

func api(method, token string) string { return telegramAPIBase + token + "/" + method }

func setMyCommandsBody() ([]byte, error) {
	return json.Marshal(map[string]any{"commands": commandNames()})
}

// watchMiniAppButton keeps the menu button pointing at an address something is
// actually serving.
//
// The button is written once, when a tunnel announces itself, and a quick tunnel
// is a child process that can be reaped by a reboot, a signal or nothing more
// sinister. When that happens the hostname keeps resolving on paper and stops
// answering in fact, and the button is left advertising it — which is what a
// person sees as Cloudflare 1033 and cannot connect back to anything here.
//
// So it is checked rather than trusted. A dead client is replaced, and a changed
// origin is published. The chat is not told, because a repair that sent a message
// every few minutes would be worse than the fault it fixes; the log records it.
func watchMiniAppButton(srv *miniapp.Server, token string) {
	ticker := time.NewTicker(miniAppWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-shutdown.Done():
			return
		case <-ticker.C:
		}
		if srv.TunnelAddress() == "" {
			// Nothing published and nothing to repair. Starting one unasked is
			// /web's decision, not this loop's.
			continue
		}
		origin, err := srv.EnsureTunnel()
		if err != nil {
			logger.Warn("Mini App watchdog: " + err.Error())
			continue
		}
		if origin == "" || origin == miniAppPublishedOrigin() {
			continue
		}
		// The tunnel moved. The button is moved to match and a fresh tap-time
		// link is pushed, because the client holding the old one is already
		// answering 1033 and no API call makes it refetch.
		republishMiniAppAddress(token, srv, origin)
	}
}

// miniAppState is the one surface this process runs, and the address its menu
// button was last given.
//
// Package level rather than local to Run because the watchdog reads the published
// address on its own schedule. It is also what makes "exactly one surface" true
// across the two paths that can open one: /web and the boot path both come
// through the same closure, and that closure is the only writer.
var (
	miniappMu        sync.Mutex
	miniappServer    *miniapp.Server
	miniappPublished string
	// miniappChat is where a republished address is announced when nobody
	// asked for it. Recorded from the chats that publish, because the only
	// signal that a URL moved is observed by this process and not by the
	// client holding a stale copy of it - the client never tells us, it just
	// renders a 1033 to a thumb.
	miniappChat int64
)

// miniAppPublishedOrigin is the address the menu button was last given.
//
// Guarded by miniappMu because the watchdog reads it on its own schedule and the
// command that publishes writes it. The two run on different goroutines, so
// reading it unguarded would be a race rather than a stale value.
func miniAppPublishedOrigin() string {
	miniappMu.Lock()
	defer miniappMu.Unlock()
	return miniappPublished
}

func noteMiniAppPublished(origin string) {
	miniappMu.Lock()
	defer miniappMu.Unlock()
	miniappPublished = origin
}

// noteMiniAppChat records the chat an address was published to, so a later
// republication has somewhere to go without being asked.
//
// Persisted, because the republish that needs it usually happens on the boot
// path - after a restart - and a boot that started from a blank state would
// have nowhere to push. The write is best effort: losing the chat costs a
// silent republish, which is no worse than having never recorded it.
func noteMiniAppChat(chatID int64) {
	if chatID == 0 {
		return
	}
	miniappMu.Lock()
	miniappChat = chatID
	miniappMu.Unlock()
	if err := defaultState.setNotifyChat(chatID); err != nil {
		logger.Warn("Mini App notify chat not persisted: " + err.Error())
	}
}

// miniAppChat is seeded from the persisted value at boot, so the in-memory
// copy and the stored one cannot disagree about where a republish goes.
func miniAppChat() int64 {
	miniappMu.Lock()
	chatID := miniappChat
	miniappMu.Unlock()
	if chatID != 0 {
		return chatID
	}
	return defaultState.notifyChat()
}

// miniAppWatchInterval is how often the tunnel is checked for having died.
//
// Long enough that a healthy surface costs nothing worth noticing, short enough
// that a dead one is repaired before someone taps the button and is told
// Cloudflare cannot find it. Two minutes is well inside the gap between opening
// the app and deciding it is broken.
const miniAppWatchInterval = 2 * time.Minute

// setChatMenuButton points the button beside the input field at url.
//
// A quick tunnel mints a new hostname every time it starts, so a URL configured
// once in BotFather is stale the moment the process restarts, and the symptom is
// Cloudflare's 1033 rather than anything that points at the cause. The button is
// the one surface Telegram lets the bot rewrite, so the address goes there and
// BotFather is never involved.
//
// Failing is not fatal: the address has already been sent to the chat, and a
// button that did not move is better than a command that reports no address at
// all. The reason is logged because a button that silently stopped following is
// exactly the bug this replaces.
func setChatMenuButton(token string, text string, url string) error {
	body, err := json.Marshal(map[string]any{
		"menu_button": map[string]any{
			"type":    "web_app",
			"text":    text,
			"web_app": map[string]any{"url": url},
		},
	})
	if err != nil {
		return err
	}
	resp, err := doTelegramPost(api("setChatMenuButton", token), body)
	if err != nil {
		return err
	}
	if resp == nil {
		return fmt.Errorf("setChatMenuButton returned no response")
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
		return fmt.Errorf("setChatMenuButton: %s", result.Description)
	}
	return nil
}

// clearChatMenuButton puts the button back to a plain label with no web app
// behind it.
//
// It exists because the button Telegram is holding outlives the process that
// set it. A quick tunnel's hostname is minted per boot, so a restart leaves the
// button pointing at a URL that is already dead, and every tap in the startup
// window answers 1033. A label that does nothing is visibly incomplete; a dead
// link reads as Ceraa being down. The boot path clears first and sets the real
// address the moment the tunnel announces, so the button is never wrong.
func clearChatMenuButton(token string, text string) error {
	body, err := json.Marshal(map[string]any{
		"menu_button": map[string]any{
			"type": "default",
			"text": text,
		},
	})
	if err != nil {
		return err
	}
	resp, err := doTelegramPost(api("setChatMenuButton", token), body)
	if err != nil {
		return err
	}
	if resp == nil {
		return fmt.Errorf("setChatMenuButton returned no response")
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
		return fmt.Errorf("setChatMenuButton: %s", result.Description)
	}
	return nil
}

// publishMiniAppAddress tells the chat where the surface is, and moves the menu
// button to match so the next launch does not depend on remembering a hostname.
//
// One place, because the address arrives twice — immediately when a tunnel is
// already up, and later when one had to be opened — and the button has to follow
// whichever happened. Sending the address and setting the button from the same
// function is what keeps them from disagreeing.
func publishMiniAppAddress(token string, chatID int64, address string) {
	noteMiniAppChat(chatID)
	send(token, chatID, "Mini App:\n"+address)
	if err := setChatMenuButton(token, "🌐 Web", address); err != nil {
		logger.Warn("Mini App menu button not updated: " + err.Error())
		return
	}
	noteMiniAppPublished(address)
	logger.Info("Mini App menu button set to " + address)
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
	model, _ := defaultState.values()
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
	return postTelegramWithTrace(url, body, methodFromURL(url))
}

// methodFromURL extracts the API method from a request URL, which is the only
// identifying part: the bot token is in the path and must never be logged.
func methodFromURL(url string) string {
	if i := strings.LastIndex(url, "/"); i >= 0 && i+1 < len(url) {
		return url[i+1:]
	}
	return "unknown"
}

// telegramHTTPError carries a rejected call's status and body.
//
// The message is byte-for-byte the one this function has always produced, so
// nothing that formats the error changes. The body is kept structured because it
// holds Telegram's own description: callers that need to tell "message is not
// modified" from a genuine rejection would otherwise be parsing JSON back out of
// a string, and every attempt to do that has read the string wrong at least once.
type telegramHTTPError struct {
	status int
	body   string
}

func (e *telegramHTTPError) Error() string {
	return fmt.Sprintf("telegram HTTP %d: %s", e.status, e.body)
}

func postTelegramWithTrace(url string, body []byte, method string) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		resp, err := telegramClient.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			lastErr = err
			if telegramTrace.Load() {
				traceAPICall(method, body, 0, "", false, "transport: "+err.Error())
			}
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
			lastErr = &telegramHTTPError{status: resp.StatusCode, body: string(raw)}
			if telegramTrace.Load() {
				traceAPICall(method, body, resp.StatusCode, string(raw), false, string(raw))
			}
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
		traceSuccessfulCall(method, body, resp)
		return resp, nil
	}
	return nil, lastErr
}

// traceSuccessfulCall records a call the API accepted. The verdict is the point:
// ok:true means Telegram accepted the request, not that a client rendered it, so a
// keyboard that was silently ignored looks exactly like a success from here.
func traceSuccessfulCall(method string, body []byte, resp *http.Response) {
	if !telegramTrace.Load() || resp == nil || resp.Body == nil {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if err != nil {
		return
	}
	// Put the body back so the caller can still read it.
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	var verdict struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(raw, &verdict)
	traceAPICall(method, body, resp.StatusCode, string(raw), verdict.OK, verdict.Description)
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
	// Named, so Back into the hub re-renders it. Replaying a snapshot left every
	// provider row showing the state it had when the hub was first opened.
	sendNamedMenuMarkup(token, chatID, "hub", "📋 Ceraa hub", menus.Hub())
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

// sendHTML sends text with Telegram HTML formatting.
//
// send() passes no parse_mode, so a message carrying <code> or <b> renders the
// tags literally. HTML mode is all-or-nothing though: one unescaped < or > makes
// Telegram reject the whole message, and interpolated values such as a model ID
// or a device code are not guaranteed to be tag-free. So a parse failure retries
// as plain text rather than dropping the message.
func sendHTML(token string, chatID int64, text string, kb map[string]any) int {
	messageID, err := sendMessagePayload(token, chatID, text, "HTML", kb)
	if err == nil {
		return messageID
	}
	if isTelegramParseError(err) {
		fallbackID, fallbackErr := sendMessagePayload(token, chatID, text, "", kb)
		if fallbackErr == nil {
			return fallbackID
		}
		logger.Warn("Telegram HTML fallback failed: " + fallbackErr.Error())
		return 0
	}
	logger.Warn("Telegram HTML sendMessage failed: " + err.Error())
	return 0
}

// sendPlainMessage carries the reply keyboard.
//
// send() was the only sender that attached it, so the answer to an actual prompt
// went out with no keyboard at all, while /help and other command replies looked
// fine. Every bot message that carries the keyboard is another carrier, and the
// keyboard only disappears once all of them are gone, which is what makes it
// survive the deletion of any single command message.
func sendPlainMessage(token string, chatID int64, text string) int {
	messageID, err := sendMessagePayload(token, chatID, text, "", keyboard.Main())
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

// activeRun is the cancellable state of the run in progress for one chat.
type activeRun struct {
	cancel context.CancelFunc
	// stop marks the renderer stopped as well as cancelling, so the run reports as
	// stopped rather than as a failure.
	stop      func()
	sessionID string
}

// stopActiveRun cancels the chat's current run, if there is one. It reports
// whether a run was found.
func stopActiveRun(chatID int64) (bool, string) {
	value, ok := activeRuns.Load(chatID)
	if !ok {
		return false, ""
	}
	run, ok := value.(*activeRun)
	if !ok || run == nil {
		return false, ""
	}
	// stop() cancels through the renderer, which holds the same cancel func, so
	// the stopped flag and the cancellation cannot disagree.
	if run.stop != nil {
		run.stop()
		return true, run.sessionID
	}
	run.cancel()
	return true, run.sessionID
}

// clearStopMessage is gone along with the companion Stop message. Cancelling is
// reached with /stop, /cancel or /abort.

// defaultRunCeiling bounds a single agent run.
//
// This used to be a flat 10 minutes, which silently truncated long-horizon work:
// a refactor across twenty files, a slow test suite, or a dependency install all
// died mid-flight with "Cancelled after timeout." and no way to tell that apart
// from a hang. The ceiling is now a leak guard set far above any real task, and
// is overridable for the rare run that genuinely needs longer.
const defaultRunCeiling = 6 * time.Hour

// runCeiling returns the per-run wall-clock limit, from CERAA_RUN_TIMEOUT when
// set to a positive duration. A malformed or non-positive value falls back to the
// default rather than disabling the guard, so a typo cannot leave a run unbounded.
func runCeiling() time.Duration {
	raw := strings.TrimSpace(envs.Get("RUN_TIMEOUT"))
	if raw == "" {
		return defaultRunCeiling
	}
	if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
		return parsed
	}
	logger.Warn("ignoring CERAA_RUN_TIMEOUT=" + raw + ": expected a positive duration such as 90m")
	return defaultRunCeiling
}

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

// shutdown is cancelled by SIGINT or SIGTERM. It is created here rather than
// per-call so the poll loop and every backoff can share one deadline, and so a
// signal arriving during a 30s long poll is acted on when the poll returns
// instead of being lost.
//
// Without it, SIGTERM hit a process whose only exit path was an unconditional
// for{}: the Go runtime terminated the process outright, so no deferred cleanup
// ran, and a Chromium launched for a browser tool call survived as an orphan.
// Signal handling is not the graceful option here, it is the only option.
var shutdown, stopSignals = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

// sleepCtx waits for d, returning false if a shutdown was requested first.
// A plain time.Sleep would make shutdown latency equal to the backoff.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// getUpdatesRequest builds the long-poll request against a caller-supplied
// context.
//
// The context is a parameter rather than an internal detail because this is the
// only part of the poll loop that cancellation can actually interrupt. getUpdates
// is a long poll the server holds open for 30s, so a loop that only checks for
// shutdown between iterations stays blocked for up to 30s after the signal
// arrives; a stop waiting 8s sees nothing, escalates to SIGKILL, and reports a
// kill against a daemon that was shutting down correctly.
//
// It is a separate function so a test can drive this exact construction. A test
// that builds its own cancellable request instead proves nothing about the
// production path, and will keep passing if this ever reverts to a background
// context.
func getUpdatesRequest(ctx context.Context, url string) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
}

func Run() error {
	// Released when Run returns, so a second Run in the same process is not
	// permanently wedged by a context that is already cancelled.
	defer stopSignals()
	// The CLI restart path: SIGUSR1 funnels into the same arm-and-shutdown
	// sequence /restart uses, so one shutdown path serves both surfaces.
	WatchRestartSignal()
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

	// Provision before the logger, because the line below is the first thing an
	// operator sees when a machine is slow to come up, and the answer to "is
	// search broken here" is usually in it. A deliberate startup step, reported
	// once, rather than a side effect of a package init nobody can see.
	provision.Report(os.Stderr)

	// Initialize logger -> maps to soraacode observability/logging.ts initializeLogger
	if _, err := logger.Initialize(""); err != nil {
		fmt.Printf("logger init failed: %v\n", err)
	} else {
		logger.Info("Ceraa bridge starting")
		if !startup.providerReady {
			logger.Info("Telegram token-only startup: provider onboarding required")
		}
		// Looked for in the background, so a release nobody has noticed is
		// visible in the log of the next boot without the boot waiting on it.
		go noteNewRelease()
		defer logger.Get().Flush()
	}

	// Initialize encrypted secret store
	keyFile := filepath.Join(paths.DataDir(), "secret.key")
	secretFile := filepath.Join(paths.DataDir(), "secrets.json")
	if err := secret.Init(keyFile, secretFile); err != nil {
		return fmt.Errorf("secret store: %w", err)
	}
	// Arm exact-value redaction with everything already stored, before a single
	// tool can run. Patterns alone only catch vendor-shaped credentials, and a
	// secret a person chose has no prefix to recognise.
	armSecretRedaction()

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
				}, nil)
			})
			if err != nil && schedulerCtx.Err() == nil {
				logger.Warn("Scheduler stopped: " + err.Error())
			}
		}()
	}
	// Report completed device logins to the chat that started them.
	registerCodexCompletion(token)
	registerHermesCompletion(token)
	registerXaiCompletion(token)
	registerMinimaxCompletion(token)
	registerClineCompletion(token)

	offset := getInitialOffset(token)

	// Claim the daemon slot before the first poll, so a second start refuses
	// rather than two daemons fighting over one Telegram offset and one state
	// file. The record is removed by the deferred cleanup below, which is why
	// registering it has to happen after the defers that remove it.
	pidPath := procfile.Path(paths.DataDir())
	if ticks, err := procfile.StartTicks(os.Getpid()); err == nil {
		rec := procfile.Record{
			PID:        os.Getpid(),
			PGID:       syscall.Getpgrp(),
			StartTicks: ticks,
			Command:    "ceraa start",
		}
		if err := procfile.Write(pidPath, rec); err != nil {
			return fmt.Errorf("claim daemon slot: %w", err)
		}
		defer procfile.Remove(pidPath)
		defer func() {
			// Close before the pid file is removed, so a stop that sees the file
			// still gone knows the browser teardown already happened.
			if closed := tool.CloseLiveBrowsers(); closed > 0 {
				logger.Info(fmt.Sprintf("Closed %d live browser(s) on shutdown", closed))
			}
		}()
		logger.Info(fmt.Sprintf("Recorded daemon pid %d (pgid %d)", rec.PID, rec.PGID))
	}

	// The Mini App surface is opt-in at boot: this is what opens the port
	// without anyone typing /web, and what puts it on the internet. It is
	// started here rather than earlier so the close defer below lands after the
	// browser teardown defer above: defers run last in, first out, and a port
	// that still accepts while the tools are being closed is a port reporting a
	// half-dead process.
	//
	// Construction lives behind one closure rather than at the call site, for
	// two reasons. There must be exactly one surface, so two paths opening it
	// would race. And /web reaches the same closure, so the command starts the
	// surface on demand instead of answering with a flag the operator has to
	// set and restart for. The gate is the same user id the bot answers to, so
	// the two surfaces can never disagree about who is admitted.
	startMiniApp := func() (*miniapp.Server, error) {
		miniappMu.Lock()
		defer miniappMu.Unlock()
		if miniappServer != nil {
			return miniappServer, nil
		}
		srv, err := buildMiniAppSurface(token, allowID)
		if err != nil {
			return nil, err
		}
		miniappServer = srv
		// One watchdog per surface, started here rather than at either caller so
		// that /web and the boot path cannot both end up with one.
		go watchMiniAppButton(srv, token)
		logger.Info("Mini App listening on " + srv.Address())
		return srv, nil
	}
	// Registered unconditionally, and before any of them can run: a surface
	// opened on demand by /web is still a surface, and one that is never closed
	// leaves a port holding a listener for a process that has already gone.
	defer func() {
		miniappMu.Lock()
		srv := miniappServer
		miniappMu.Unlock()
		if srv == nil {
			return
		}
		if err := srv.Close(); err != nil {
			logger.Warn("Mini App shutdown: " + err.Error())
		}
	}()
	if miniapp.Enabled() {
		srv, err := startMiniApp()
		switch {
		case err != nil:
			logger.Warn("Mini App disabled: " + err.Error())
		case miniapp.TunnelEnabled():
			// The public front is a separate decision from opening the port,
			// because this one puts the port on the internet. It is opened in
			// the background: a cold handshake is seconds of wall clock and the
			// poll loop below still has not started.
			//
			// The button is moved here as well as in /web. A tunnel opened at
			// boot has a hostname nobody has been told yet, and a stale one from
			// a previous boot is the failure this whole arrangement removes.
			//
			// The stale one is cleared before the handshake rather than left to
			// be overwritten by it. A quick tunnel's hostname lives only as long
			// as the process that minted it, so the button Telegram is still
			// holding after a restart points at a URL that is already dead, and
			// it keeps pointing there for the whole of the startup window. That
			// window is seconds on a good boot and a minute on a bad one, and
			// either way every second of it serves 1033. The button is taken
			// back to a plain label first, so it is never wrong, only briefly
			// incomplete.
			if err := clearChatMenuButton(token, "🌐 Web (starting)"); err != nil {
				logger.Warn("Mini App menu button not cleared: " + err.Error())
			}
			go func() {
				address, err := srv.StartTunnel()
				if err != nil {
					logger.Warn("Mini App tunnel unavailable: " + err.Error())
					return
				}
				logger.Info("Mini App shared at " + address)
				if err := setChatMenuButton(token, "🌐 Web", address); err != nil {
					logger.Warn("Mini App menu button not updated: " + err.Error())
					return
				}
				noteMiniAppPublished(address)
				logger.Info("Mini App menu button set to " + address)
				// A restart mints a new hostname, so every client still holding
				// the old button is one tap from a 1033. Push a tap-time link
				// to the chat that is known to be in use, so the fastest way
				// back in does not depend on the cache being refreshed.
				if chatID := miniAppChat(); chatID != 0 {
					sendMessageWithMarkup(token, chatID, "Fresh address, tap to open:", purgeKeyboard(address))
				}
			}()
		}
	}

	fmt.Printf("telegram polling start offset=%d\n", offset)
	// A restart that nobody confirms looks identical to a crash from the
	// chat side, so the new image says it is back. Async so a slow send can
	// never hold the poll loop open.
	go sendRestartNotice(token)
	// The poll loop is an unconditional for{} with no exit, so before a signal
	// handler existed the deferred scheduler cancel and log flush above could
	// never run and the process had to be killed outright. Every clean shutdown
	// path depends on this check, and on every sleep in the loop becoming
	// interruptible, because the 30s long-poll timeout is the worst-case latency
	// between a signal arriving and the loop noticing.
	for {
		if shutdown.Err() != nil {
			logger.Info("Shutdown requested; stopping Telegram polling")
			return nil
		}
		req, reqErr := getUpdatesRequest(shutdown,
			api(fmt.Sprintf("getUpdates?timeout=30&offset=%d&allowed_updates=%s", offset, "%5B%22message%22%2C%22callback_query%22%5D"), token))
		var resp *http.Response
		var err error
		if reqErr != nil {
			err = reqErr
		} else {
			resp, err = telegramPollClient.Do(req)
		}
		if err != nil {
			if shutdown.Err() != nil {
				logger.Info("Shutdown interrupted long poll; stopping Telegram polling")
				return nil
			}
			logger.Warn("Telegram getUpdates request failed: " + err.Error())
			if !sleepCtx(shutdown, 2*time.Second) {
				logger.Info("Shutdown during backoff; stopping Telegram polling")
				return nil
			}
			continue
		}
		raw, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			logger.Warn("Telegram getUpdates response read failed: " + readErr.Error())
			if !sleepCtx(shutdown, 2*time.Second) {
				return nil
			}
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
			if !sleepCtx(shutdown, 2*time.Second) {
				return nil
			}
			continue
		}
		if !out.Ok && out.ErrorCode == 0 {
			logger.Warn("Telegram getUpdates returned an unsuccessful response")
			if !sleepCtx(shutdown, 2*time.Second) {
				return nil
			}
			continue
		}
		if out.ErrorCode == 409 {
			fmt.Printf("conflict 409 - backing off 5s: %s\n", out.Description)
			if !sleepCtx(shutdown, 5*time.Second) {
				return nil
			}
			continue
		}
		if !out.Ok && out.ErrorCode != 0 {
			fmt.Printf("poll error %d: %s\n", out.ErrorCode, out.Description)
			if !sleepCtx(shutdown, 2*time.Second) {
				return nil
			}
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
					defaultState.noteUpload(u.Message.Chat.ID, "photo", path, "")
					send(token, u.Message.Chat.ID, "📷 Photo saved: "+path+" — send a prompt describing what to do with it, it will be attached via harness/tool view_image")
				} else {
					send(token, u.Message.Chat.ID, "📷 Photo received: "+telegramFilePreview(fileID)+" (download failed: "+err.Error()+")")
				}
				continue
			}
			if u.Message.Document != nil {
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				if path, err := downloadTelegramFile(token, u.Message.Document.FileID); err == nil {
					defaultState.noteUpload(u.Message.Chat.ID, "document", path, u.Message.Document.FileName)
					send(token, u.Message.Chat.ID, "📄 Document saved: "+path+" ("+u.Message.Document.FileName+") — send a prompt to read it: zips unpack with a member listing, PDF/DOCX/PPTX come back as text")
				} else {
					send(token, u.Message.Chat.ID, "📄 Document: "+u.Message.Document.FileName+" (download failed)")
				}
				continue
			}
			if u.Message.Voice != nil {
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				// Voice -> STT stub maps to soraacode stt-service.js whisper-large-v3-turbo
				if path, err := downloadTelegramFile(token, u.Message.Voice.FileID); err == nil {
					defaultState.noteUpload(u.Message.Chat.ID, "voice", path, "")
					send(token, u.Message.Chat.ID, "🎤 Voice saved: "+path+" — STT via whisper not yet wired, send text prompt instead")
				} else {
					send(token, u.Message.Chat.ID, "🎤 Voice received (download failed)")
				}
				continue
			}
			if u.Message.Text == "" {
				// An armed capture must never be skipped by the silence here. If the
				// value arrives in a message this path does not recognise as text, the
				// one safe thing to do is drop the message and say so; letting it
				// through would not store it either, but it would also leave the user
				// waiting with no explanation.
				if secretCaptureArmed(u.Message.Chat.ID) {
					GetDefaultInbox().CheckAndRecord(u.UpdateID)
					name, _ := takePendingSecret(u.Message.Chat.ID)
					secretFlowLog(u.Message.Chat.ID, "nontext", fmt.Sprintf("name=%q dropped", name))
					send(token, u.Message.Chat.ID, fmt.Sprintf(
						"That message arrived without text, so %s was not stored. "+
							"Reply to this message with the value as plain text.", name))
				}
				continue
			}
			txt := normalizeCommandText(strings.TrimSpace(u.Message.Text))
			setMenuTriggerPending(u.Message.Chat.ID, u.Message.MessageID)
			// A secret capture is checked first, and before anything that could put
			// the text anywhere durable. The value must not reach the session file, a
			// transcript, or a tool result, so this has to run ahead of
			// handlePendingInput as well as ahead of the run.
			if handleSecretInput(token, u.Message.Chat.ID, int64(u.Message.MessageID), txt) {
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				continue
			}
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
			case "/apilog":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleAPILog(token, u.Message.Chat.ID, strings.TrimPrefix(txt, "/apilog"))
			case "/keyboard", "/keyboard_reset":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleKeyboard(token, u.Message.Chat.ID)
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
			case "/schedule":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleScheduleCommand(token, u.Message.Chat.ID, txt)
			case "/cache":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleCacheCommand(token, u.Message.Chat.ID)
			case "/context", "/context_control":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleContextCommand(token, u.Message.Chat.ID)
			case "/permission":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handlePermissionCommand(token, u.Message.Chat.ID)
			case "/settings":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleSettings(token, u.Message.Chat.ID)
			case "/abort", "/stop", "/cancel":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleAbort(token, u.Message.Chat.ID)
			case "/restart":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleRestart(token, u.Message.Chat.ID)
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
			case "/web":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handleWebShare(token, u.Message.Chat.ID, startMiniApp)
			case "/purge":
				GetDefaultInbox().CheckAndRecord(u.UpdateID)
				handlePurge(token, u.Message.Chat.ID, startMiniApp)
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
	// comes from the encrypted native store at request time. Comparing against
	// "hermes" alone meant a working Codex install read as unconfigured, so /start
	// answered with the onboarding picker and startup announced that onboarding
	// was still required.
	if llm.IsSubscription(provider.Subscription) {
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
	prompt = withUploadContext(chatID, prompt)
	runCtx, cancelRun := context.WithCancel(context.Background())
	renderer := newRunRenderer(token, chatID)
	renderer.setCancel(cancelRun)
	defer renderer.close()

	// Cancelling is by command only: /stop, /cancel or /abort. A companion Stop
	// button was tried and removed; the typing indicator already shows that work is
	// happening, so the extra message was noise.
	active := &activeRun{
		cancel:    cancelRun,
		stop:      renderer.stop,
		sessionID: sessionID,
	}
	activeRuns.Store(chatID, active)
	defer activeRuns.Delete(chatID)

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
						// One token, three buttons. The request id, the tool name and
						// the decision words all used to be inlined here, which put
						// callback_data at 71 to 78 bytes against a 64 byte limit, and
						// Telegram rejects the entire message rather than the button.
						// The prompt therefore never appeared, and the tool sat on its
						// five minute timeout with nothing on screen to answer.
						sendWithKeyboard(token, chatID,
							"🔒 Permission required for `"+tool+"`\n"+summariseToolRequest(tool, e.Data["args"]),
							permissionKeyboard(pendingCallback{SessionID: sessionID, RequestID: reqID, Tool: tool}))
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
						kb := questionKeyboard(pendingCallback{SessionID: sessionID, RequestID: reqID}, opts, freeForm)
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
		}, renderer.markReasoningOnly)
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
	case <-time.After(runCeiling()):
		typing.Stop()
		cancelRun()
		<-doneCh
		close(eventDone)
		renderer.fail()
		send(token, chatID, "Cancelled after "+runCeiling().String()+". Raise CERAA_RUN_TIMEOUT if this task legitimately runs longer.")
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
	// Save to the files directory, which survives reboots unlike /tmp. The name
	// below is Telegram's, not the sender's: two uploads with the same remote
	// name overwrite each other, and the save notice carries the original.
	dir := paths.FilesDir()
	_ = os.MkdirAll(dir, 0o755)
	dst := filepath.Join(dir, filepath.Base(out.Result.FilePath))
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return "", err
	}
	return dst, nil
}

func runWithThinkingContext(ctx context.Context, sessionID string, prompt string, onContent func(string), onReasoning func(string), onTool func(string, string), onAnswer func(string), onStepReasoningOnly func()) error {
	return runAgentContext(ctx, sessionID, prompt, onContent, onReasoning, onTool, onAnswer, onStepReasoningOnly)
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
	if txt == "/skills_catalog" {
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
	// Captured before the flow starts: by the time the id is known the poller may
	// already have finished, and the owner has to be registered in one write.
	activeAtStart := defaultState.provider()
	flow, err := hermesAuthMgr.Start(context.Background(), "nous")
	if err != nil {
		send(token, chatID, "Failed to start Hermes login: "+err.Error())
		return
	}
	hermesLoginModels.Store(flow.ID, model)
	// Record the owner so the completion path can post the outcome to this chat
	// and activate the provider. A typed command used to be invisible to it.
	trackHermesFlow(flow.ID, chatID, model != "", activeAtStart)
	keyboard, _ := flow.InlineKeyboardJSON()
	var markup map[string]any
	if err := json.Unmarshal(keyboard, &markup); err != nil {
		send(token, chatID, "Failed to encode Hermes login menu: "+err.Error())
		return
	}
	// Hermes renders <code> in its login message too, so it needs HTML mode for
	// the same reason.
	sendHTML(token, chatID, flow.ResultMessage(), markup)
}

// handleHermesStatus reports whether a Nous Portal credential is stored and what
// this chat has in flight.
//
// It never changes the active provider. It used to: it walked every Hermes flow
// on the host and activated on the first that had succeeded, so typing
// /hermes-status switched the model answering underneath the conversation. The
// sign-in watchers already refuse an activation whose switch was superseded, and
// this path never went through them at all. handleCodexStatus has always been
// only a query, which is what this is now.
//
// Credential state is read first because flows are transient. Sign in yesterday
// and no flow exists today, which the old code answered with "no completed login"
// for a credential that was sitting there working. The question being asked is
// whether Hermes can be used, not whether a flow object happens to be resident.
func handleHermesStatus(token string, chatID int64) {
	if hermesauth.SignedIn() {
		if defaultState.provider() == llm.HermesSubscription {
			hermesNotify(token, chatID, "✅ Nous Portal credential stored. Hermes is the active provider.", hermesStatusKeyboard(true))
			return
		}
		hermesNotify(token, chatID, "✅ Nous Portal credential stored. Another provider is active.", hermesStatusKeyboard(true))
		return
	}
	if flow := hermesFlowForChat(chatID); flow != nil {
		if status, _, userCode := flow.StatusSnapshot(); status == "waiting" || status == "checking" {
			hermesNotify(token, chatID, "🔄 Hermes login in progress. Code: "+userCode, nil)
			return
		}
	}
	hermesNotify(token, chatID, "❌ No Nous Portal credential stored. Use /hermes-login to sign in.", hermesStatusKeyboard(false))
}

func activateHermesProvider(token string, chatID int64, flowID string) {
	model := hermesModelForAccount(flowID)
	model, err := guardModel("hermes", model)
	if err != nil {
		logger.Warn("Hermes activate: " + err.Error())
		send(token, chatID, "Hermes activation failed: "+err.Error())
		return
	}
	// ContextLimit left unset so the catalog supplies the model's real window.
	//
	// The URL here is for display, not for requests: agent.go resolves the Hermes
	// credential per run and uses credential.BaseURL, which the portal may point
	// somewhere other than the default. Recording the default keeps /status and the
	// provider menu honest without pretending to be the effective endpoint.
	profile := config.ProviderProfile{URL: llm.HermesBaseURL, Model: model, MaxOutputTokens: 8192, PromptCacheMode: "auto", Subscription: llm.HermesSubscription, HermesProvider: "nous"}
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
	// Captured before the flow starts; see handleHermesLogin.
	activeAtStart := defaultState.provider()
	state, err := authMgr.Start()
	if err != nil {
		send(token, chatID, "Failed to start Codex login: "+err.Error())
		return
	}
	// Registered before the menu is even built, so the watcher can post the
	// outcome to this chat without the user having to tap Check. A login started
	// from the typed command goes through here too, which is what it did not do
	// before: pendingCodexActivation was only set by the provider menu, so a
	// command-started login could never finish on its own.
	//
	// Also first for timing: the poller starts with the flow and answers on a
	// loop, so no file read belongs between Start and this registration.
	trackCodexFlow(state.Snapshot().FlowID, chatID, true, activeAtStart)
	kb, _ := state.InlineKeyboardJSON()
	var markup map[string]any
	if err := json.Unmarshal(kb, &markup); err != nil {
		send(token, chatID, "Failed to encode Codex login menu: "+err.Error())
		return
	}
	sendHTML(token, chatID, codexProgressHTML(state.Snapshot()), markup)
}

// codexStatusKeyboard is the action row for a Codex status report.
//
// The status replies used to instruct the user to type /codex-login. That is a
// dead end for anyone driving the bot from its menus, which is the only surface
// available on a phone, so every state now carries the button that does whatever
// the text would have asked for.
func codexStatusKeyboard(authenticated, active bool) map[string]any {
	row := func(text, data string) map[string]string {
		return map[string]string{"text": text, "callback_data": data}
	}
	switch {
	case !authenticated:
		return map[string]any{"inline_keyboard": [][]map[string]string{
			{row("🔑 Sign in to ChatGPT", "codex:login")},
		}}
	case !active:
		return map[string]any{"inline_keyboard": [][]map[string]string{
			{row("✅ Switch to Codex", "codex:activate"), row("🔁 Sign in again", "codex:login")},
		}}
	default:
		return map[string]any{"inline_keyboard": [][]map[string]string{
			{row("🔁 Sign in again", "codex:login")},
		}}
	}
}

func handleCodexStatus(token string, chatID int64) {
	// A stored token is the real answer, rather than a marker a login happened to
	// leave behind. It is not on its own proof that inference works, so the
	// message says which provider is actually active: a stored credential with
	// another provider selected is not a usable Codex setup.
	authenticated := codexauth.Authenticated()
	active := defaultState.provider() == llm.CodexSubscription
	_, activeModel := defaultState.values()
	// The runtime override above is empty unless the user set one, so it reports
	// a blank model and hides which model is actually answering. The profile
	// backing the active provider is the source of truth.
	if _, profile, ok := defaultState.activeProviderProfile(); ok && profile.Model != "" {
		activeModel = profile.Model
	}
	if activeModel == "" {
		activeModel = "unknown"
	}
	// Which branch ran is the first question when a status reply looks stale, and
	// it is invisible from the message alone.
	logger.Info("codex status: authenticated=" + strconv.FormatBool(authenticated) +
		" active_provider=" + defaultState.provider() + " active_model=" + activeModel +
		" expires=" + codexExpiryLabel())

	// A login already in flight is reported with that flow's own keyboard, so the
	// buttons offered match the state the user is actually in.
	if flow := codexFlowForChat(chatID); flow != nil {
		state := flow.Snapshot()
		switch state.Status {
		case codexauth.StatusWaiting, codexauth.StatusApproved:
			markup, err := flow.InlineKeyboardJSON()
			if err == nil {
				var kb map[string]any
				if json.Unmarshal(markup, &kb) == nil {
					sendHTML(token, chatID, codexProgressHTML(state), kb)
					return
				}
			}
		}
	}

	if authenticated && active {
		sendHTML(token, chatID,
			"✅ Codex is authenticated and active.\nModel: <code>"+html.EscapeString(activeModel)+"</code>\nAccess token expires: "+codexExpiryLabel(),
			codexStatusKeyboard(true, true))
		return
	}
	if authenticated {
		sendHTML(token, chatID,
			"✅ Codex is authenticated (access token valid until "+codexExpiryLabel()+").\n\n"+
				"It is not the active provider — Ceraa is using <b>"+html.EscapeString(defaultState.provider())+"</b>.",
			codexStatusKeyboard(true, false))
		return
	}
	sendHTML(token, chatID,
		"❌ Not authenticated. Codex needs a ChatGPT sign-in before it can be used.",
		codexStatusKeyboard(false, false))
}

// codexProgressHTML renders an in-flight login for the chat that started it.
func codexProgressHTML(state codexauth.State) string {
	head := "🔄 Sign-in in progress.\nOpen the link, approve the code, and this will finish on its own."
	if state.UserCode != "" {
		head += "\n\nCode: <code>" + html.EscapeString(state.UserCode) + "</code>"
	}
	if state.Hint != "" {
		head += "\n\n" + html.EscapeString(state.Hint)
	}
	return head
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
	// Captured before the flow starts; see handleHermesLogin.
	activeAtStart := defaultState.provider()
	flow, err := authMgr.Start()
	if err != nil {
		send(token, chatID, "Failed to start Codex login: "+err.Error())
		return
	}
	// Registered first, for the reason handleCodexLogin gives.
	trackCodexFlow(flow.Snapshot().FlowID, chatID, true, activeAtStart)
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
	sendHTML(token, chatID,
		"Sign in to ChatGPT to use Codex. Open the link, approve the code, and Ceraa will switch to Codex on its own — no need to press Check.\n\n"+codexProgressHTML(flow.Snapshot()),
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
	// Every outcome goes through codexNotify, so a caller that already reported a
	// completed login does not produce two different-looking messages, and a
	// failure is still delivered with a Codex keyboard rather than the main menu.
	//
	// The failure branches matter: the credential is already stored by the time
	// activation runs, so failing to switch leaves a usable Codex behind an
	// inactive provider. Saying so, with the switch button attached, is the only
	// way the user finds out.
	if err := defaultState.setProviderProfile(llm.CodexSubscription, profile); err != nil {
		codexNotify(token, chatID,
			"⚠️ Codex sign-in succeeded and the credential is stored, but saving the Codex profile failed: "+
				html.EscapeString(err.Error())+"\n\nCodex is not active. Use the button below to try again.",
			codexStatusKeyboard(true, false))
		return
	}
	if err := defaultState.setProvider(llm.CodexSubscription); err != nil {
		codexNotify(token, chatID,
			"⚠️ Codex sign-in succeeded and the credential is stored, but switching the active provider failed: "+
				html.EscapeString(err.Error())+"\n\nUse the button below to try again.",
			codexStatusKeyboard(true, false))
		return
	}
	pendingCodexActivation.Delete(chatID)
	codexNotify(token, chatID, "✅ Codex provider activated with model <code>"+html.EscapeString(model)+"</code>.\nSend a task to use it.", nil)
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
	_, model := defaultState.values()
	if llm.IsCodexModel(model) {
		return model
	}
	return ""
}

// hermesModelForAccount picks the model to activate.
//
// An explicit choice from the login command wins. Otherwise the currently active
// model is reused only if it is one the Nous Portal actually serves, because
// carrying over, say, an apmix model id would produce a profile the endpoint
// rejects outright. Same rule as codexModelForAccount.
// hermesModelForAccount returns the model to activate the provider with.
//
// It used to return (flowID, model) and every caller destructured it as
// (model, _), so activation wrote the login flow id — an id of the shape
// "minimax-1790571540-1" — into the provider profile. That value then went
// out as the model id, into the model-identity block, and into the pricing
// lookup, which reported model_not_found and priced nothing. One value
// means the mistake cannot be made again.
func hermesModelForAccount(flowID string) string {
	if flowID != "" {
		if value, ok := hermesLoginModels.Load(flowID); ok {
			if model, _ := value.(string); model != "" {
				return model
			}
		}
	}
	current, _ := defaultState.values()
	if llm.IsHermesModel(current) {
		return current
	}
	return llm.DefaultHermesModel
}

// selectHermesProvider is the Hermes entry point, mirroring selectCodexProvider.
//
// A stored credential switches provider immediately; otherwise a device login
// starts. Both paths go through trackHermesFlow, so approving the code on the
// Nous portal page is enough: the completion callback activates the provider
// without the user coming back to press anything.
func selectHermesProvider(token string, chatID int64) {
	selectHermesProviderImpl(token, chatID)
}

// selectHermesProviderImpl is the real entry point.
//
// Indirected so a test can assert the provider menu actually routes
// provider:hermes here, without starting a device login against the live Nous
// portal. The button existing and being handled are separate facts, and only the
// first was covered.
var selectHermesProviderImpl = func(token string, chatID int64) {
	if hermesauth.SignedIn() {
		// No flow id, because there is no in-flight login to attach the activation
		// to. The profile is written directly instead.
		activateHermesProvider(token, chatID, "")
		return
	}
	startHermesLogin(token, chatID)
}

// startHermesLogin begins a Hermes device login whether or not a credential
// exists. Reachable only from an explicit "Sign in again"; see startMinimaxLogin.
func startHermesLogin(token string, chatID int64) {
	// Captured before the flow starts; see handleHermesLogin.
	activeAtStart := defaultState.provider()
	flow, err := hermesAuthMgr.Start(context.Background(), "nous")
	if err != nil {
		send(token, chatID, "Failed to start Hermes login: "+err.Error())
		return
	}
	// activate: true, because reaching Hermes from the provider menu is a request
	// to use it. A typed /hermes-status is a request to look, not to switch.
	trackHermesFlow(flow.ID, chatID, true, activeAtStart)
	keyboard, err := flow.InlineKeyboardJSON()
	if err != nil {
		send(token, chatID, "Failed to encode Hermes login menu: "+err.Error())
		return
	}
	var markup map[string]any
	if json.Unmarshal(keyboard, &markup) != nil {
		send(token, chatID, "Failed to encode Hermes login menu.")
		return
	}
	sendHTML(token, chatID,
		"Sign in to Nous Portal to use Hermes. Open the link, approve the code, and Ceraa will switch to Hermes on its own — no need to press Check.\n\n"+flow.ResultMessage(),
		markup)
}

// selectXaiProvider is the xAI entry point, mirroring selectCodexProvider.
//
// A stored credential switches provider immediately; otherwise a device login
// starts. Reaching xAI from the provider menu asks to use it, so the flow is
// tracked with activate set and the completion callback switches provider on its
// own once the code is approved.
func selectXaiProvider(token string, chatID int64) { xaiEntryPoint(token, chatID) }

// xaiEntryPoint is the real xAI entry point, indirected so a test can assert the
// provider menu routes provider:xai here without starting a device login against
// the live xAI issuer.
var xaiEntryPoint = func(token string, chatID int64) {
	if xaiauth.SignedIn() {
		activateXaiProvider(token, chatID, "")
		return
	}
	startXaiLogin(token, chatID)
}

// startXaiLogin begins an xAI device login whether or not a credential exists.
// Reachable only from an explicit "Sign in again"; see startMinimaxLogin.
func startXaiLogin(token string, chatID int64) {
	// Captured before the flow starts; see handleHermesLogin.
	activeAtStart := defaultState.provider()
	flow, err := xaiLogin.Start(context.Background())
	if err != nil {
		send(token, chatID, "Failed to start xAI login: "+err.Error())
		return
	}
	snapshot := flow.Snapshot()
	trackXaiFlow(snapshot.ID, chatID, true, activeAtStart)
	sendHTML(token, chatID,
		"Sign in to xAI to use Grok. Open the link, approve the code, and Ceraa will switch to xAI on its own — no need to press Check.\n\n"+
			"🔐 xAI Device Login\n\n1. Open: "+snapshot.VerificationURL+"\n2. Enter code: <code>"+snapshot.UserCode+"</code>\n3. Approve in the browser, then it activates by itself",
		xaiLoginKeyboard(flow))
}

func xaiLoginKeyboard(flow *oauthdevice.Flow) map[string]any {
	snapshot := flow.Snapshot()
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "🔗 Open Authorization URL", "url": snapshot.VerificationURL}},
		{{"text": "✗ Cancel", "callback_data": "xai:cancel:" + snapshot.ID}},
	}}
}

// activateXaiProvider saves the xAI profile and makes it active.
func activateXaiProvider(token string, chatID int64, flowID string) {
	model := xaiModelForAccount(flowID)
	model, err := guardModel("xai", model)
	if err != nil {
		logger.Warn("xAI activate: " + err.Error())
		send(token, chatID, "xAI activation failed: "+err.Error())
		return
	}
	// The profile URL is for display: agent.go resolves the credential per run and
	// uses its own base URL, which is the same host.
	profile := config.ProviderProfile{
		URL:             llm.XaiBaseURL,
		Model:           model,
		ContextLimit:    llm.XaiContextWindow(model),
		MaxOutputTokens: llm.XaiMaxOutputTokens,
		Subscription:    llm.XAISubscription,
	}
	if err := defaultState.setProviderProfile("xai", profile); err != nil {
		send(token, chatID, "xAI profile save failed: "+err.Error())
		return
	}
	if err := defaultState.setProvider("xai"); err != nil {
		send(token, chatID, "xAI provider activation failed: "+err.Error())
		return
	}
	send(token, chatID, "✅ xAI/Grok provider activated with model "+model+". Send a task to use it.")
}

// xaiModelForAccount picks the model to activate.
//
// Reuses the active model only when it is one the Grok endpoint is known to
// serve, so a model left over from another provider cannot produce a profile the
// endpoint rejects. Same rule as the Codex and Hermes paths.
// xaiModelForAccount returns the model to activate the provider with.
//
// It used to return (flowID, model) and every caller destructured it as
// (model, _), so activation wrote the login flow id — an id of the shape
// "minimax-1790571540-1" — into the provider profile. That value then went
// out as the model id, into the model-identity block, and into the pricing
// lookup, which reported model_not_found and priced nothing. One value
// means the mistake cannot be made again.
func xaiModelForAccount(flowID string) string {
	current, _ := defaultState.values()
	if llm.IsXaiModel(current) {
		return current
	}
	return llm.DefaultXaiModel
}

// selectMinimaxProvider is the MiniMax entry point, mirroring the other
// subscription providers.
//
// A stored credential switches provider immediately; otherwise a device login
// starts. Reaching MiniMax from the provider menu asks to use it, so the flow is
// tracked with activate set and the completion callback switches provider on its
// own once the code is approved.
func selectMinimaxProvider(token string, chatID int64) {
	minimaxEntryPoint(token, chatID)
}

// minimaxEntryPoint is the real entry point, indirected so a test can assert the
// provider menu routes provider:minimax here without starting a device login
// against the live service.
var minimaxEntryPoint = func(token string, chatID int64) {
	signedIn := minimaxauth.SignedIn(minimaxauth.RegionGlobal)
	// Which branch this took was unobservable, and a wrong branch here leaves the
	// active provider pointing somewhere else while the menu says otherwise.
	logger.Info("minimax entry: signed_in=" + strconv.FormatBool(signedIn) +
		" active_before=" + defaultState.provider())
	if signedIn {
		activateMinimaxProvider(token, chatID, "")
		return
	}
	startMinimaxLogin(token, chatID)
}

// startMinimaxLogin begins a device login, whether or not a credential exists.
//
// It is reachable only from an explicit request. "Sign in again" and "Switch to"
// both arrived here, so tapping either one while a credential was stored took the
// activate branch and silently re-activated instead of re-authenticating, and
// removing the provider did not help because removal clears the profile while the
// credential lives in the encrypted store. Re-authentication was therefore
// unreachable by any route.
func startMinimaxLogin(token string, chatID int64) {
	// Captured before the flow starts; see handleHermesLogin.
	activeAtStart := defaultState.provider()
	flow, err := minimaxLogin.Start(context.Background())
	if err != nil {
		send(token, chatID, "Failed to start MiniMax login: "+err.Error())
		return
	}
	snapshot := flow.Snapshot()
	trackMinimaxFlow(snapshot.ID, chatID, true, activeAtStart)
	sendHTML(token, chatID,
		"Sign in to MiniMax to use it. Open the link, enter the code, approve, and Ceraa will switch to MiniMax on its own — no need to press Check.\n\n"+
			"🔐 MiniMax Device Login\n\n1. Open: "+snapshot.VerificationURL+"\n2. Enter code: <code>"+snapshot.UserCode+"</code>\n3. Approve in the browser, then it activates by itself",
		map[string]any{"inline_keyboard": [][]map[string]string{
			{{"text": "🔗 Open Authorization URL", "url": snapshot.VerificationURL}},
			{{"text": "✗ Cancel", "callback_data": "minimax:cancel:" + snapshot.ID}},
		}})
}

// activateMinimaxProvider saves the MiniMax profile and makes it active.
func activateMinimaxProvider(token string, chatID int64, flowID string) {
	model := minimaxModelForAccount(flowID)
	model, err := guardModel("minimax", model)
	if err != nil {
		logger.Warn("MiniMax activate: " + err.Error())
		send(token, chatID, "MiniMax activation failed: "+err.Error())
		return
	}
	// The profile URL is for display: agent.go resolves the credential per run and
	// uses its own base URL, which is the same host.
	profile := config.ProviderProfile{
		URL:             llm.MiniMaxBaseURL,
		Model:           model,
		MaxOutputTokens: 8192,
		PromptCacheMode: "auto",
		Subscription:    llm.MiniMaxSubscription,
	}
	if err := defaultState.setProviderProfile("minimax", profile); err != nil {
		logger.Warn("minimax activate: profile save failed: " + err.Error())
		send(token, chatID, "MiniMax profile save failed: "+err.Error())
		return
	}
	if err := defaultState.setProvider("minimax"); err != nil {
		logger.Warn("minimax activate: provider set failed: " + err.Error())
		send(token, chatID, "MiniMax provider activation failed: "+err.Error())
		return
	}
	logger.Info("minimax activate: provider=" + defaultState.provider() + " model=" + model)
	send(token, chatID, "✅ MiniMax provider activated with model "+model+". Send a task to use it.")
}

// minimaxModelForAccount picks the model to activate.
//
// Reuses the active model only when it is one the subscription serves, so a model
// left over from another provider cannot produce a profile the endpoint rejects.
// minimaxModelForAccount returns the model to activate the provider with.
//
// It used to return (flowID, model) and every caller destructured it as
// (model, _), so activation wrote the login flow id — an id of the shape
// "minimax-1790571540-1" — into the provider profile. That value then went
// out as the model id, into the model-identity block, and into the pricing
// lookup, which reported model_not_found and priced nothing. One value
// means the mistake cannot be made again.
func minimaxModelForAccount(flowID string) string {
	current, _ := defaultState.values()
	if llm.IsMiniMaxModel(current) {
		return current
	}
	return llm.DefaultMiniMaxModel
}

// guardModel refuses to activate a provider with no model.
//
// A profile with an empty model is not a degraded provider, it is an unusable
// one: the request goes out with no model id and the endpoint answers with
// something unrelated to the actual fault. One of these reached the store during
// development, so the write is refused here rather than trusted to every caller.
// It used to check only for an empty string, while the four activation paths
// treated it as the last thing standing between a bad model and a live profile.
// Nothing checked that the provider actually served the value, so a login flow
// id written by an activation bug survived and went out as the model id.
func guardModel(providerID, model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", fmt.Errorf("cannot activate %s with no model", providerID)
	}
	if !llm.IsSubscriptionModel(providerID, model) {
		// A wrong model is recoverable, an absent one is not, so only this case
		// falls back. Logging keeps the substitution visible in the log rather
		// than silent.
		logger.Warn(providerID + " activation: " + model + " is not served by this provider")
		model = llm.DefaultModelFor(providerID)
		if model == "" {
			return "", fmt.Errorf("cannot activate %s: it serves no known model", providerID)
		}
	}
	return model, nil
}

// selectClineProvider is the Cline Account entry point, mirroring the others.
//
// A stored credential switches provider immediately; otherwise a device login
// starts. Reaching Cline from the provider menu asks to use it, so the flow is
// tracked with activate set and the completion callback switches provider on its
// own once the code is approved.
func selectClineProvider(token string, chatID int64) {
	clineEntryPoint(token, chatID)
}

// selectOpenCodeProvider either activates a Zen profile that already has a key
// or starts the two-step onboarding that asks for one.
func selectOpenCodeProvider(token string, chatID int64) {
	openCodeEntryPoint(token, chatID)
}

// openCodeEntryPoint is indirected the same way clineEntryPoint is, so a test
// can assert the provider menu routes provider:opencode here without reaching for
// a live endpoint.
var openCodeEntryPoint = func(token string, chatID int64) {
	if opencodeauth.SignedIn() {
		activateOpenCodeProvider(token, chatID, "")
		return
	}
	startOpenCodeOnboarding(token, chatID)
}

// startOpenCodeOnboarding asks for a key and nothing else, reusing the shared
// provider onboarding state machine rather than a flow of its own.
//
// It starts at "key" rather than at "model". The base URL is filled in rather
// than prompted for: Zen has one documented root and asking the user for it
// invites a near-miss on the path, which fails as a 404 instead of as a wrong
// key. The model is the same argument one stage further along. Zen serves 84 of
// them, an id is not something a person invents, and a free-form prompt here
// puts the catalog question in the one place that cannot show the catalog. The
// picker under Model lists all of them and marks the active one, so this screen
// only has to establish that a credential exists.
func startOpenCodeOnboarding(token string, chatID int64) {
	pendingProvider.Store(chatID, openCodeDraft())
	send(token, chatID, "Send your OpenCode Zen API key, or type cancel.\n\nPick the model afterwards from the Model menu.")
}

// openCodeDraft is the onboarding state, split out so the stage it starts at can
// be asserted without a send. The stage is the whole behaviour: starting at
// "model" is what put a free-form model prompt in front of someone who was only
// here to paste a key.
func openCodeDraft() providerDraft {
	return providerDraft{
		ID:    llm.OpenCodeSubscription,
		URL:   llm.OpenCodeBaseURL,
		Stage: "key",
	}
}

// activateOpenCodeProvider saves the Zen profile and makes it active.
//
// Reached when a key is already stored, so the only thing left to settle is the
// model. A model carried over from another provider is not assumed to exist on
// Zen: the endpoint serves a fixed list and a wrong id is a hard 400, so an
// unknown one is replaced by the default rather than sent.
func activateOpenCodeProvider(token string, chatID int64, model string) {
	if model == "" {
		if profile, ok := currentProfile(llm.OpenCodeSubscription); ok {
			model = profile.Model
		}
	}
	if !llm.IsOpenCodeModel(model) {
		model = llm.DefaultOpenCodeModel
	}
	model, err := guardModel(llm.OpenCodeSubscription, model)
	if err != nil {
		logger.Warn("OpenCode activate: " + err.Error())
		send(token, chatID, "OpenCode activation failed: "+err.Error())
		return
	}
	profile := config.ProviderProfile{
		URL:          llm.OpenCodeBaseURL,
		Model:        model,
		Subscription: llm.OpenCodeSubscription,
	}
	if err := defaultState.setProviderProfile(llm.OpenCodeSubscription, profile); err != nil {
		send(token, chatID, "OpenCode profile save failed: "+err.Error())
		return
	}
	if err := defaultState.setProvider(llm.OpenCodeSubscription); err != nil {
		send(token, chatID, "OpenCode provider activation failed: "+err.Error())
		return
	}
	send(token, chatID, "🌌 OpenCode Zen activated with model "+model+". Send a task to use it.")
}

// clineEntryPoint is the real entry point, indirected so a test can assert the
// provider menu routes provider:cline here without starting a device login
// against the live service.
var clineEntryPoint = func(token string, chatID int64) {
	if clineauth.SignedIn() {
		activateClineProvider(token, chatID, "")
		return
	}
	startClineLogin(token, chatID)
}

// startClineLogin begins a Cline device login whether or not a credential exists.
// Reachable only from an explicit "Sign in again"; see startMinimaxLogin.
func startClineLogin(token string, chatID int64) {
	// Captured before the flow starts; see handleHermesLogin.
	activeAtStart := defaultState.provider()
	flow, err := clineLogin.Start(context.Background())
	if err != nil {
		send(token, chatID, "Failed to start Cline login: "+err.Error())
		return
	}
	snapshot := flow.Snapshot()
	trackClineFlow(snapshot.ID, chatID, true, activeAtStart)
	sendHTML(token, chatID,
		"Sign in to your Cline Account to use the Cline gateway. Open the link, enter the code, approve, and Ceraa will switch on its own — no need to press Check.\n\n"+
			"🔐 Cline Account Login\n\n1. Open: "+snapshot.VerificationURL+"\n2. Enter code: <code>"+snapshot.UserCode+"</code>\n3. Approve in the browser, then it activates by itself",
		map[string]any{"inline_keyboard": [][]map[string]string{
			{{"text": "🔗 Open Authorization URL", "url": snapshot.VerificationURL}},
			{{"text": "✗ Cancel", "callback_data": "cline:cancel:" + snapshot.ID}},
		}})
}

// activateClineProvider saves the Cline profile and makes it active.
func activateClineProvider(token string, chatID int64, flowID string) {
	model := clineModelForAccount(flowID)
	model, err := guardModel("cline", model)
	if err != nil {
		logger.Warn("Cline activate: " + err.Error())
		send(token, chatID, "Cline activation failed: "+err.Error())
		return
	}
	// The profile URL is the gateway root; the chat transport appends the path.
	profile := config.ProviderProfile{
		URL:             llm.ClineGatewayBase,
		Model:           model,
		MaxOutputTokens: 8192,
		PromptCacheMode: "auto",
		Subscription:    llm.ClineSubscription,
	}
	if err := defaultState.setProviderProfile("cline", profile); err != nil {
		send(token, chatID, "Cline profile save failed: "+err.Error())
		return
	}
	if err := defaultState.setProvider("cline"); err != nil {
		send(token, chatID, "Cline provider activation failed: "+err.Error())
		return
	}
	send(token, chatID, "✅ Cline activated with model "+model+". Send a task to use it.")
}

// clineModelForAccount picks the model to activate.
//
// Reuses the active model only when it is one the gateway's free tier serves, so
// a model left over from another provider cannot produce a profile the gateway
// rejects.
// clineModelForAccount returns the model to activate the provider with.
//
// It used to return (flowID, model) and every caller destructured it as
// (model, _), so activation wrote the login flow id — an id of the shape
// "minimax-1790571540-1" — into the provider profile. That value then went
// out as the model id, into the model-identity block, and into the pricing
// lookup, which reported model_not_found and priced nothing. One value
// means the mistake cannot be made again.
func clineModelForAccount(flowID string) string {
	current, _ := defaultState.values()
	if llm.IsClineModel(current) {
		return current
	}
	return llm.DefaultClineModel
}

// selectSubscriptionModel writes a chosen model into a subscription profile.
//
// The model catalog is a shared list keyed on OpenAI-compatible hosts, so it
// never carried these providers' models and there was no way to change the
// default one a provider activated with. The lists behind this picker are the
// same ones the transports validate against, so a choice cannot be one the
// endpoint rejects.
//
// Switching is left to a separate press: picking a model is a common thing to do
// while comparing, and making every tap also change the answering provider would
// surprise someone mid-sentence.
func selectSubscriptionModel(token string, chatID int64, providerID, model string) {
	marker := subscriptionMarkerFor(providerID)
	if marker == "" {
		send(token, chatID, "Unknown provider: "+providerID)
		return
	}
	// Only a model this provider is known to serve, so a crafted callback cannot
	// write a profile the endpoint will reject.
	if !llm.IsSubscriptionModel(marker, model) {
		send(token, chatID, "That is not a "+providerID+" model.")
		return
	}
	profile := config.ProviderProfile{Model: model, Subscription: marker}
	if existing, ok := currentProfile(providerID); ok {
		profile = existing
		profile.Model = model
		profile.Subscription = marker
	}
	profile.MaxOutputTokens = 8192
	profile.PromptCacheMode = "auto"
	if err := defaultState.setProviderProfile(providerID, profile); err != nil {
		send(token, chatID, "Model save failed: "+err.Error())
		return
	}
	active := defaultState.provider()
	verb := "saved for"
	if active == providerID {
		verb = "applied to"
	}
	send(token, chatID, "✅ Model "+model+" "+verb+" "+providerID+". Use 🔌 Provider to switch to it.")
}

// subscriptionMarkerFor maps a profile id to its subscription marker.
func subscriptionMarkerFor(providerID string) string {
	switch providerID {
	case "codex":
		return llm.CodexSubscription
	case "hermes":
		return llm.HermesSubscription
	case "xai":
		return llm.XAISubscription
	case "minimax":
		return llm.MiniMaxSubscription
	case "cline":
		return llm.ClineSubscription
	case "opencode":
		return llm.OpenCodeSubscription
	default:
		return ""
	}
}

// currentProfile reads a provider's stored profile.
//
// Reads config, which is where provider profiles live. It used to reach into the
// bridge state instead, with a comment explaining that reading the config file
// "returned nothing" — the profiles were in the other file, so the config reader
// could not see them. Both copies are now one, and this reads the same one the
// run path reads.
func currentProfile(providerID string) (config.ProviderProfile, bool) {
	return providers.profiles()[providerID], providers.hasProfile(providerID)
}

// buildMiniAppSurface constructs and opens the Mini App surface.
//
// Separate from the decision to open it, because two paths now open it: boot,
// when the flag is set, and /web, when someone asks for the link. Built in one
// place they share the same session, allow list and settings; built twice one
// of them would eventually drift and the two would disagree about who is
// admitted or which transcript the page writes to.
func buildMiniAppSurface(token string, allowID int64) (*miniapp.Server, error) {
	store, err := configuredStore()
	if err != nil {
		return nil, fmt.Errorf("session store unavailable: %w", err)
	}
	cfg := miniapp.Config{
		Token:     token,
		AllowID:   allowID,
		Store:     store,
		Bus:       event.Default,
		SessionID: miniapp.WebSessionID,
		// Its own session, and callbacks that say nothing: the reply reaches
		// the page on the event bus as structured deltas, not as a line a
		// renderer was asked to print. Reusing the chat session here would put
		// two frontends in one transcript.
		Prompt: func(ctx context.Context, text string) error {
			return runAgentContext(ctx, miniapp.WebSessionID, text, nil, nil, nil, nil, nil)
		},
		AnswerPermission: func(requestID, decision string) bool {
			return permission.Default.Answer(miniapp.WebSessionID, requestID, decision)
		},
		AnswerQuestion: func(requestID, answer string) bool {
			return question.Default.Answer(miniapp.WebSessionID, requestID, answer)
		},
		Settings:    webSettings,
		SetSettings: applyWebSettings,
	}
	// The dashboard's backend. Wired before New copies the config, with the
	// tunnel address resolving late through the holder: the tunnel does not
	// exist until Start runs, and the status function must ask the live server
	// rather than the address at build time. One line per concern inside
	// webControl, so a missing wire is visible there rather than as a 503
	// discovered from the page.
	var surface *miniapp.Server
	webControl(&cfg, func() string {
		if surface == nil {
			return ""
		}
		return surface.TunnelAddress()
	})
	srv := miniapp.New(cfg)
	surface = srv
	if err := srv.Start(); err != nil {
		return nil, err
	}
	return srv, nil
}
