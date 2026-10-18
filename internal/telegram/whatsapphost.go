package telegram

// WhatsApp hosting: the bridge in ceraa/internal/whatsapp owns transport
// and per-chat runs, and this file owns the two things only the daemon
// can provide: a RunFunc wired like a Telegram run, and the boot hook
// that starts the bridge beside the poll loop.
//
// The shape mirrors runAgentWithConfigContextAt above, minus the parts
// that belong to a Telegram chat: no live-run registration (no Telegram
// chat owns a WhatsApp run, so there is nothing to bind an actor to),
// no keepalive recipe (the warmer stays Telegram's; a WhatsApp recipe
// recorded here would clobber it), no plan mode (there is no chat state
// to read one from). send_media IS registered here, bound to the chat the
// run belongs to: the sender lives in the whatsapp package and needs the
// live whatsmeow client, which only the daemon holds, so the tool is
// wired on this side of the seam the way it is for Telegram.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ceraa/harness/coordinator"
	"ceraa/harness/evaluation"
	"ceraa/harness/event"
	"ceraa/harness/llm"
	"ceraa/harness/operation"
	"ceraa/harness/permission"
	"ceraa/harness/sessionstore"
	"ceraa/harness/skills"
	"ceraa/harness/tool"
	"ceraa/internal"
	"ceraa/internal/config"
	"ceraa/internal/logger"
	"ceraa/internal/paths"
	"ceraa/internal/prompt"
	"ceraa/internal/subagent"
	"ceraa/internal/telegram/menus"
	"ceraa/internal/whatsapp"

	"go.mau.fi/whatsmeow"
)

// whatsappRunFunc adapts the bridge inlet to the full run path. The
// bridge builds SessionID plus WhatsApp-flavored callbacks; everything
// else (provider, stores, registry, authorization) is settled here,
// the same way runAgentWithConfigContextAt settles it for Telegram.
func whatsappRunFunc(cfg *config.Config) whatsapp.RunFunc {
	return func(ctx context.Context, rc coordinator.RunnerConfig, userText string) (coordinator.Result, error) {
		sessionID := rc.SessionID
		if cfg.SkillFile == "" {
			cfg.SkillFile = "skills"
		}
		if cfg.EvaluationFile == "" {
			cfg.EvaluationFile = "evaluations.json"
		}
		store, err := sessionstore.New(cfg.SessionFile)
		if err != nil {
			return coordinator.Result{}, err
		}
		skillStore, err := skills.New(cfg.SkillFile)
		if err != nil {
			return coordinator.Result{}, err
		}
		evaluationStore, err := evaluation.New(cfg.EvaluationFile)
		if err != nil {
			return coordinator.Result{}, err
		}
		projectRoot := strings.TrimSpace(os.Getenv("CERAA_WHATSAPP_PROJECT"))
		if projectRoot == "" {
			// Unknown to the Telegram state, so this falls through to
			// the configured workspace root. One chat surface, one
			// default; per-chat projects are phase two.
			projectRoot = defaultState.projectForSession(sessionID)
		}
		providerID := cfg.EffectiveProviderID()
		providerConfig := activeProviderConfig(cfg, providerID)
		modelConfig := llm.FromProvider(providerConfig.URL, providerConfig.APIKey, providerConfig.Model)
		modelConfig.PromptCacheMode = providerConfig.PromptCacheMode
		modelConfig.PromptCacheTTL = providerConfig.PromptCacheTTL
		modelConfig.GeminiThoughtSignatures = telegramProviderUsesGeminiThoughtSignatures(providerID, providerConfig)
		compactor := newCompactor(cfg, providerID, sessionID)
		resolveModel := providerModelResolver(providerConfig, sessionID)
		compactor.ResolveModel = resolveModel

		skillContext, _ := skillStore.CatalogContext(2000)
		reasoning := llm.ReasonLevel(effectiveReasoning(providerConfig, nil))
		logger.Info(fmt.Sprintf("whatsapp run: provider=%s model=%s session=%s", providerID, providerConfig.Model, sessionID))
		// DefaultRegistryForUser, not telegramRegistry: the Telegram
		// registry carries a send_media bound to the Telegram chat, and
		// a WhatsApp run delivering files to Telegram would be a
		// surprise. Children share the same registry through
		// NewRegistry for the same reason.
		//
		// send_media is then registered here, bound to this run's chat,
		// which is the only change from Telegram's shape: the tool is
		// the same, the destination is not.
		registry := tool.DefaultRegistryForUser(userText, skillStore)
		if sender := waMediaSender(rc.ChatID); sender != nil {
			registry.Register(&tool.SendMediaTool{Sender: sender})
		}
		subagent.Attach(registry, &subagent.Factory{
			Cfg:             cfg,
			User:            userText,
			ParentSession:   sessionID,
			ProjectRoot:     projectRoot,
			ProviderID:      providerID,
			ProviderCfg:     providerConfig,
			ResolveModel:    resolveModel,
			Reasoning:       reasoning,
			ParentPrompt:    prompt.System,
			SkillStore:      skillStore,
			SkillContext:    skillContext,
			EvaluationStore: evaluationStore,
			Store:           store,
			WallClock:       operation.TaskTimeout,
			NewRegistry: func(user string, skillStore *skills.Store) *tool.Registry {
				registry := tool.DefaultRegistryForUser(user, skillStore)
				// A child delivers to the same chat as its parent,
				// so it gets the same bound sender. Omitting it
				// would make send_media work for the parent and
				// not for the subagent that needed it.
				if sender := waMediaSender(rc.ChatID); sender != nil {
					registry.Register(&tool.SendMediaTool{Sender: sender})
				}
				return registry
			},
		})
		instructionProposer, _ := internal.ProposeScopes(modelConfig)

		call := func(fn func(string)) func(string) {
			if fn == nil {
				return func(string) {}
			}
			return fn
		}
		onContent, onReasoning, onDone := call(rc.Callbacks.OnContent), call(rc.Callbacks.OnReasoning), call(rc.Callbacks.OnDone)
		onToolStart, onToolDone := rc.Callbacks.OnToolStart, rc.Callbacks.OnToolDone
		onStarted, onFinished := call(rc.Callbacks.OnRunStarted), rc.Callbacks.OnRunFinished
		full := rc
		full.Reasoning = reasoning
		full.PlanMode = false
		full.SystemPrompt = prompt.System
		full.Model = modelConfig
		full.ResolveModel = resolveModel
		full.ContextLimit = providerConfig.ContextLimit
		full.MaxOutputTokens = providerConfig.MaxOutputTokens
		full.WorkDir = projectRoot
		full.ToolOutputDir = cfg.ToolOutputDir
		full.MaxToolResultChars = cfg.MaxToolResultChars
		full.MaxConcurrentTools = cfg.MaxConcurrentTools
		full.GoalDir = cfg.SessionFile
		full.ModelCatalogURL = cfg.ModelCatalogURL
		full.ModelCatalogCache = cfg.ModelCatalogCache
		full.ModelCatalogRefresh = time.Duration(cfg.ModelCatalogRefreshHours) * time.Hour
		full.InstructionRules = internal.InstructionRules(projectRoot, instructionProposer)
		full.SkillContext = skillContext
		full.EvaluationStore = evaluationStore
		full.Compactor = compactor
		full.ForcedCompaction = func() bool { return consumeForcedCompaction(sessionID) }
		full.NeedsAuthorization = func(name string, args map[string]any) bool {
			return needsPermission(name, fmt.Sprint(args))
		}
		full.AuthorizeTool = func(authCtx context.Context, callID, name string, args map[string]any) (bool, error) {
			decision, decisionErr := permission.Default.Create(authCtx, sessionID, callID, name, 5*time.Minute, func() {
				event.Default.Publish(event.Event{Type: event.TypePermission, SessionID: sessionID, Data: map[string]any{"tool": name, "args": args, "request_id": callID}})
			})
			if decisionErr != nil {
				return false, decisionErr
			}
			return decision == "allowOnce" || decision == "allowAlways", nil
		}
		// Bus publishes mirror agentRunnerConfig so a WhatsApp run is
		// visible on every subscriber (Mini App included), then the
		// bridge callback carries it to the chat.
		full.Callbacks.OnContent = func(content string) {
			event.Default.Publish(event.Event{Type: event.TypeContent, SessionID: sessionID, Data: map[string]any{"delta": content}})
			onContent(content)
		}
		full.Callbacks.OnReasoning = func(reasoning string) {
			event.Default.Publish(event.Event{Type: event.TypeThinking, SessionID: sessionID, Data: map[string]any{"delta": reasoning}})
			onReasoning(reasoning)
		}
		if rc.Callbacks.OnStepReasoningOnly != nil {
			full.Callbacks.OnStepReasoningOnly = rc.Callbacks.OnStepReasoningOnly
		}
		full.Callbacks.OnToolStart = func(callID, name string, args map[string]any) {
			event.Default.Publish(event.Event{Type: event.TypeToolStart, SessionID: sessionID, Data: map[string]any{"call_id": callID, "tool": name, "args": args}})
			if onToolStart != nil {
				onToolStart(callID, name, args)
			}
		}
		full.Callbacks.OnToolDone = func(callID, name string, result operation.ResultEvent) {
			event.Default.Publish(event.Event{Type: event.TypeToolEnd, SessionID: sessionID, Data: map[string]any{
				"call_id": callID, "tool": name, "status": string(result.Status), "outcome": toolResultLabel(result),
			}})
			if onToolDone != nil {
				onToolDone(callID, name, result)
			}
		}
		full.Callbacks.OnDone = func(answer string) {
			if answer != "" {
				event.Default.Publish(event.Event{Type: event.TypeComplete, SessionID: sessionID, Data: map[string]any{"text": answer}})
			}
			onDone(answer)
		}
		full.Callbacks.OnRunStarted = func(runID string) {
			logger.Info(fmt.Sprintf("whatsapp run active session=%s run=%s", sessionID, runID))
			onStarted(runID)
		}
		full.Callbacks.OnRunFinished = func(runID string, status sessionstore.RunStatus) {
			logger.Info(fmt.Sprintf("whatsapp run finished session=%s run=%s", sessionID, runID))
			if onFinished != nil {
				onFinished(runID, status)
			}
		}
		// No CacheWarmer: the keepalive recipe is daemon-level and the
		// recorded one belongs to Telegram. Recording a WhatsApp recipe
		// here would clobber it, so WhatsApp sessions run without a
		// keepalive until they get their own.
		runner := coordinator.New(full, store, registry)
		return runner.Run(ctx, userText)
	}
}

// WhatsApp host state. The daemon owns one transport and at most one
// manager for the whole process; the hub reaches them through these
// accessors, because the menus package cannot see the daemon and must not
// open the session database the daemon holds open.
var (
	waMu        sync.Mutex
	waCfg       *config.Config
	waTransport *whatsapp.ChatTransport
	waManager   *whatsapp.Manager
)

// startWhatsAppBridge prepares the WhatsApp surface at boot. An enabled
// bridge with a paired session starts serving immediately; an enabled but
// unpaired bridge idles until the hub's Link button drives pairing, so
// first pairing happens on demand instead of as a QR storm on every boot.
// A failure never fails the boot: Telegram is the primary surface.
func startWhatsAppBridge(cfg *config.Config) {
	wcfg := whatsapp.LoadConfig()
	waMu.Lock()
	waCfg = cfg
	waMu.Unlock()
	if !wcfg.Enabled {
		_ = whatsapp.SaveStatus(whatsapp.Status{})
		return
	}
	transport, err := whatsapp.NewChatTransport(context.Background(), wcfg)
	if err != nil {
		logger.Warn("WhatsApp disabled: " + err.Error())
		_ = whatsapp.SaveStatus(whatsapp.Status{})
		return
	}
	waMu.Lock()
	waTransport = transport
	waMu.Unlock()
	if transport.SelfJID() == "" {
		// Idle until the hub links it. No QR is issued here on purpose:
		// the user opens the hub when they are ready to scan.
		waSaveLinkStatus(transport)
		logger.Info("WhatsApp bridge ready, not linked - open the hub to pair")
		return
	}
	if err := waStartManager(cfg, transport, wcfg); err != nil {
		logger.Warn("WhatsApp disabled: " + err.Error())
	}
	waSaveLinkStatus(transport)
}

// waStartManager connects and begins serving. The caller must have
// reserved any QR channel before this, because Start is what connects.
func waStartManager(cfg *config.Config, transport *whatsapp.ChatTransport, wcfg whatsapp.Config) error {
	mgr := whatsapp.NewManager(transport, wcfg, whatsappRunFunc(cfg), "")
	if err := mgr.Start(context.Background()); err != nil {
		return err
	}
	waMu.Lock()
	waManager = mgr
	waMu.Unlock()
	logger.Info("WhatsApp bridge started")
	return nil
}

// stopWhatsApp ends the inlet loop and disconnects. Idempotent.
func stopWhatsApp() {
	waMu.Lock()
	mgr := waManager
	waManager = nil
	waMu.Unlock()
	if mgr != nil {
		mgr.Stop()
	}
}

func waTransportSnapshot() *whatsapp.ChatTransport {
	waMu.Lock()
	defer waMu.Unlock()
	return waTransport
}

// waMediaSender returns a media sender bound to one chat, or nil when the
// transport is not up. A nil is skipped rather than registered broken: a
// tool the model can see and that fails on every call is the failure this
// seam exists to prevent.
func waMediaSender(chat string) tool.MediaSender {
	transport := waTransportSnapshot()
	if transport == nil {
		return nil
	}
	return transport.MediaSender(chat)
}

func waCfgSnapshot() *config.Config {
	waMu.Lock()
	defer waMu.Unlock()
	return waCfg
}

// waSaveLinkStatus records the link state the menus render. A save
// failure only costs a stale hub label until the next successful write.
func waSaveLinkStatus(transport *whatsapp.ChatTransport) {
	status := whatsapp.Status{}
	// Paired is a fact about the stored session, not about the current
	// socket: IsLoggedIn is false on a fresh boot even with a saved
	// device, which would report a linked account as unlinked.
	if transport != nil && transport.SelfJID() != "" {
		status.Linked = true
		status.JID = transport.SelfJID()
	}
	if err := whatsapp.SaveStatus(status); err != nil {
		logger.Warn("WhatsApp status save: " + err.Error())
	}
}

// whatsAppStatusText is the body the hub shows above the pairing buttons.
func whatsAppStatusText() string {
	if !whatsapp.LoadConfig().Enabled {
		return "📱 WhatsApp\n\nBridge is off. Set CERAA_WHATSAPP_ENABLED=true and restart to enable it."
	}
	allow := len(whatsapp.EffectiveAllowList())
	allowLine := fmt.Sprintf("\n\nAllow-list: %d number(s).", allow)
	if allow == 0 {
		allowLine = "\n\nAllow-list is empty, so nobody can reach Ceraa. Tap Allow a number and add the phone you message from."
	}
	status := whatsapp.LoadStatus()
	if status.Linked {
		jid := status.JID
		if jid == "" {
			jid = "this device"
		}
		return "📱 WhatsApp\n\nLinked as " + jid + "." + allowLine
	}
	return "📱 WhatsApp\n\nNot linked yet. Tap Link with QR code, then scan it from the phone:\nWhatsApp > Settings > Linked devices > Link a device." + allowLine
}

// whatsAppLinkNow starts pairing on demand and delivers the QR codes to
// the chat that asked. It reserves the QR channel before connecting,
// because the library requires it ahead of Connect.
func whatsAppLinkNow(token string, chatID int64) {
	if chatID == 0 {
		return
	}
	wcfg := whatsapp.LoadConfig()
	if !wcfg.Enabled {
		send(token, chatID, "WhatsApp is off. Set CERAA_WHATSAPP_ENABLED=true and restart.")
		return
	}
	transport := waTransportSnapshot()
	if transport == nil {
		created, err := whatsapp.NewChatTransport(context.Background(), wcfg)
		if err != nil {
			send(token, chatID, "WhatsApp start failed: "+err.Error())
			return
		}
		waMu.Lock()
		waTransport = created
		waMu.Unlock()
		transport = created
	}
	if transport.SelfJID() != "" {
		waSaveLinkStatus(transport)
		send(token, chatID, "WhatsApp is already linked. Tap Refresh to see it.")
		return
	}
	cfg := waCfgSnapshot()
	if cfg == nil {
		send(token, chatID, "WhatsApp is not ready yet. Restart the daemon and try again.")
		return
	}
	// A manager from an earlier attempt would fight over the socket.
	stopWhatsApp()
	qrChan, err := transport.PairingClient().GetQRChannel(context.Background())
	if err != nil {
		send(token, chatID, "WhatsApp pairing could not start: "+err.Error())
		return
	}
	if err := waStartManager(cfg, transport, wcfg); err != nil {
		send(token, chatID, "WhatsApp start failed: "+err.Error())
		return
	}
	send(token, chatID, "📱 Starting WhatsApp linking. Scan the QR code that follows: WhatsApp > Settings > Linked devices > Link a device.")
	go pairWhatsApp(token, chatID, transport, qrChan)
}

// whatsAppLinkWithPhone runs the 8-char code flow: the number is entered on
// the phone instead of a QR being scanned. It needs a live socket, so the
// manager connects first and the code is requested once the connection is
// up, then a watcher flips the status when the phone completes the link.
func whatsAppLinkWithPhone(token string, chatID int64, phone string) {
	if chatID == 0 {
		return
	}
	wcfg := whatsapp.LoadConfig()
	if !wcfg.Enabled {
		send(token, chatID, "WhatsApp is off. Set CERAA_WHATSAPP_ENABLED=true and restart.")
		return
	}
	digits, err := whatsapp.NormalizePhone(phone)
	if err != nil {
		// Keep the capture open so a mistyped number can be corrected
		// without going back to the menu.
		pendingWhatsAppPhone.Store(chatID, true)
		send(token, chatID, "That number is not usable: "+err.Error()+".\n\nSend it in international format, digits only, e.g. 2348012345678, or send cancel.")
		return
	}
	transport := waTransportSnapshot()
	if transport == nil {
		created, cerr := whatsapp.NewChatTransport(context.Background(), wcfg)
		if cerr != nil {
			send(token, chatID, "WhatsApp start failed: "+cerr.Error())
			return
		}
		waMu.Lock()
		waTransport = created
		waMu.Unlock()
		transport = created
	}
	if transport.SelfJID() != "" {
		waSaveLinkStatus(transport)
		send(token, chatID, "WhatsApp is already linked. Tap Refresh in the hub to see it.")
		return
	}
	cfg := waCfgSnapshot()
	if cfg == nil {
		send(token, chatID, "WhatsApp is not ready yet. Restart the daemon and try again.")
		return
	}
	// A manager from an earlier attempt would fight over the socket, and the
	// code flow must not have a QR channel open.
	stopWhatsApp()
	if err := waStartManager(cfg, transport, wcfg); err != nil {
		send(token, chatID, "WhatsApp start failed: "+err.Error())
		return
	}
	// WaitForConnection waits for IsLoggedIn, which stays false until the
	// link completes, so it can never pass during onboarding and the code
	// was never requested on any network. The code flow needs a live
	// socket, not a session: gate on the socket the request actually uses.
	if !waitForWaSocket(transport.PairingClient(), 20*time.Second) {
		send(token, chatID, "WhatsApp did not connect, so the pairing code was not requested. Try again in a moment.")
		return
	}
	code, err := whatsapp.RequestPairCodeLinked(context.Background(), transport.PairingClient(), digits)
	if err != nil {
		logger.Warn("WhatsApp pair code: " + err.Error())
		send(token, chatID, "WhatsApp refused the pairing code: "+err.Error()+"\n\nCheck the number and try again.")
		return
	}
	send(token, chatID, fmt.Sprintf("🔢 Pairing code for +%s:\n\n%s\n\nOn the phone: WhatsApp > Settings > Linked devices > Link a device > Link with phone number instead, then type the code.\n\nIt expires in about two minutes; tap Link with phone number again if it lapses.", digits, code))
	go awaitWhatsAppPhoneLink(transport, token, chatID)
}

// waitForWaSocket polls until the client reports a live socket or the
// deadline passes. It exists because the only wait the library offers,
// WaitForConnection, also requires IsLoggedIn, so it is unusable before a
// link exists - which is exactly when pairing needs a connection.
func waitForWaSocket(client *whatsmeow.Client, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if client.IsConnected() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return client.IsConnected()
}

// awaitWhatsAppPhoneLink watches for the phone completing the code entry.
// There is no QR channel to read, so it polls the session until it is
// paired. It gives up on the transport if a newer link attempt replaced
// it, so a stale watcher never reports a link it no longer owns.
func awaitWhatsAppPhoneLink(transport *whatsapp.ChatTransport, token string, chatID int64) {
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		if waTransportSnapshot() != transport {
			return
		}
		if transport.SelfJID() != "" {
			waSaveLinkStatus(transport)
			send(token, chatID, "✅ WhatsApp linked. Message the linked number from your phone to talk to Ceraa.")
			return
		}
	}
	if waTransportSnapshot() != transport {
		return
	}
	send(token, chatID, "The pairing code lapsed before the link completed. Open the hub and tap Link with phone number again.")
}

// whatsAppUnlink logs the device out, clears the local session and
// returns the bridge to the unlinked idle state.
func whatsAppUnlink(token string, chatID int64) {
	if chatID == 0 {
		return
	}
	waMu.Lock()
	mgr := waManager
	waManager = nil
	transport := waTransport
	waTransport = nil
	waMu.Unlock()
	if transport != nil && transport.IsLoggedIn() {
		// Logout needs the live socket, so it runs before the disconnect.
		if err := transport.PairingClient().Logout(context.Background()); err != nil {
			logger.Warn("WhatsApp logout: " + err.Error())
		}
	}
	if mgr != nil {
		mgr.Stop()
	}
	if transport != nil {
		_ = transport.Close()
	}
	if err := os.Remove(whatsapp.LoadConfig().DBPath()); err != nil && !os.IsNotExist(err) {
		logger.Warn("WhatsApp session delete: " + err.Error())
	}
	_ = whatsapp.SaveStatus(whatsapp.Status{})
	send(token, chatID, "🔓 WhatsApp unlinked. Remove the Ceraa entry in the phone's Linked devices too, then link again from the hub.")
}

// whatsAppAddAllowed stores one number in the hub-editable allow-list. An
// invalid value re-arms the prompt so a mistyped number is retried instead of
// dropped. This is the fix for the silent-nobody state: it lets the owner list
// the phone they send from without editing an env file or restarting.
func whatsAppAddAllowed(token string, chatID int64, raw string) {
	if chatID == 0 {
		return
	}
	entry, added, err := whatsapp.AddAllowListEntry(raw)
	if err != nil {
		// A mistyped number is the common case, so the capture stays open
		// rather than sending the person back to the menu.
		pendingWhatsAppAllow.Store(chatID, true)
		send(token, chatID, "That number is not usable: "+err.Error()+".\n\nSend it in international format, digits only, e.g. 2348012345678, or send cancel.")
		return
	}
	note := "✅ Allowed " + entry + "."
	if !added {
		note = "👥 " + entry + " was already allowed."
	}
	if status := whatsapp.LoadStatus(); status.JID != "" && whatsapp.SameNumber(status.JID, entry) {
		note += "\n\nNote: that is the linked account itself, whose own messages Ceraa skips. Allow the phone you message from instead."
	}
	note += "\n\n" + whatsAppAllowSummary()
	sendMenuMarkup(token, chatID, note, menus.InlineForChat("whatsapp", 0, chatID, currentSessionID(chatID)))
}

// whatsAppRemoveAllowed drops one number from the hub-editable allow-list. The
// environment half is never touched, so a number the operator locked in stays
// allowed; the callback re-renders the screen from the store either way.
func whatsAppRemoveAllowed(token string, chatID int64, raw string) {
	if chatID == 0 {
		return
	}
	if _, err := whatsapp.RemoveAllowListEntry(raw); err != nil {
		send(token, chatID, "Could not update the allow-list: "+err.Error())
	}
}

// whatsAppAllowSummary renders the effective allow-list on one line for the
// confirmation message.
func whatsAppAllowSummary() string {
	entries := whatsapp.EffectiveAllowList()
	if len(entries) == 0 {
		return "Allow-list is empty, so nobody can reach Ceraa yet."
	}
	return "Allow-list: " + strings.Join(entries, ", ")
}

// pairWhatsApp delivers pairing QR codes to the owner Telegram chat until
// the link completes or the window lapses. One channel, one attempt: a
// lapsed window needs the Link button again, which the notice says.
func pairWhatsApp(token string, chatID int64, transport *whatsapp.ChatTransport, qrChan <-chan whatsmeow.QRChannelItem) {
	if chatID == 0 {
		logger.Warn("WhatsApp not linked and no owner chat: set the Telegram user id so the QR code has somewhere to go")
		return
	}
	pngPath := filepath.Join(paths.DataDir(), "whatsapp-qr.png")
	sender := newTelegramMediaSender(token, chatID)
	for item := range qrChan {
		switch item.Event {
		case "code":
			png, err := whatsapp.RenderQRPNG(item.Code)
			if err != nil {
				logger.Warn("WhatsApp QR render: " + err.Error())
				continue
			}
			if err := os.WriteFile(pngPath, png, 0o600); err != nil {
				logger.Warn("WhatsApp QR save: " + err.Error())
				continue
			}
			caption := "Scan with WhatsApp: Settings > Linked devices > Link a device."
			if item.Timeout > 0 {
				caption += fmt.Sprintf(" This code expires in about %s; a fresh one follows automatically.", item.Timeout.Round(time.Second))
			}
			if _, err := sender.SendMedia(context.Background(), pngPath, caption, tool.MediaPhoto); err != nil {
				logger.Warn("WhatsApp QR delivery: " + err.Error())
			}
		case "success":
			_ = os.Remove(pngPath)
			waSaveLinkStatus(transport)
			send(token, chatID, "✅ WhatsApp linked. Message the linked number from your phone to talk to Ceraa.")
			return
		case "timeout":
			send(token, chatID, "WhatsApp linking lapsed before the scan. Tap Link with QR code again.")
			return
		case "error":
			if item.Error != nil {
				logger.Warn("WhatsApp pairing: " + item.Error.Error())
			}
			send(token, chatID, "WhatsApp pairing failed. Tap Link with QR code to retry.")
			return
		default:
			// Re-issues arrive as fresh "code" events; anything else
			// is logged and ignored.
			logger.Info("WhatsApp pairing event: " + item.Event)
		}
	}
}
