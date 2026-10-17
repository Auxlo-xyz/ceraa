package telegram

import (
	"ceraa/harness/llm"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ceraa/harness/permission"
	"ceraa/harness/scheduler"
	"ceraa/harness/skills"
	"ceraa/harness/todo"
	"ceraa/harness/tool"
	"ceraa/internal/buddyauth"
	"ceraa/internal/codexauth"
	"ceraa/internal/config"
	"ceraa/internal/logger"
	"ceraa/internal/secret"
	"ceraa/internal/telegram/menus"
)

func answerCallback(token, id string) {
	if id == "" {
		return
	}
	body, _ := json.Marshal(map[string]string{"callback_query_id": id})
	go func() {
		resp, err := doTelegramPost(api("answerCallbackQuery", token), body)
		if err != nil {
			logger.Warn("Telegram answerCallbackQuery failed: " + err.Error())
		}
		discardTelegramResponse(resp)
	}()
}

// editCallback replaces a message's text and keyboard.
//
// Like send(), it sets no parse_mode, so markup in the text renders literally.
// HTML mode is all-or-nothing and an unescaped < makes Telegram reject the whole
// edit, so a parse failure falls back to plain text rather than leaving the old
// message on screen.
func editCallback(token string, chatID int64, messageID int, text string, markup map[string]any) {
	editMessageText(token, chatID, messageID, text, markup, "")
}

func editHTML(token string, chatID int64, messageID int, text string, markup map[string]any) {
	editMessageText(token, chatID, messageID, text, markup, "HTML")
}

func editMessageText(token string, chatID int64, messageID int, text string, markup map[string]any, parseMode string) {
	payload := map[string]any{"chat_id": chatID, "message_id": messageID}
	if text != "" {
		payload["text"] = text
		if parseMode != "" {
			payload["parse_mode"] = parseMode
		}
	}
	if markup == nil {
		payload["reply_markup"] = map[string]any{"inline_keyboard": [][]any{}}
	} else {
		payload["reply_markup"] = markup
	}
	body, _ := json.Marshal(payload)
	resp, err := doTelegramPost(api("editMessageText", token), body)
	if err == nil {
		discardTelegramResponse(resp)
		return
	}
	if parseMode != "" && isTelegramParseError(err) {
		fallback := map[string]any{"chat_id": chatID, "message_id": messageID}
		if text != "" {
			fallback["text"] = text
		}
		if markup == nil {
			fallback["reply_markup"] = map[string]any{"inline_keyboard": [][]any{}}
		} else {
			fallback["reply_markup"] = markup
		}
		fallbackBody, _ := json.Marshal(fallback)
		fallbackResp, fallbackErr := doTelegramPost(api("editMessageText", token), fallbackBody)
		if fallbackErr == nil {
			discardTelegramResponse(fallbackResp)
			return
		}
		err = fallbackErr
	}
	logger.Warn("Telegram editMessageText failed: " + err.Error())
	discardTelegramResponse(resp)
}

func removeCallbackMarkup(token string, chatID int64, messageID int, text string) {
	emptyMarkup := map[string]any{"inline_keyboard": [][]any{}}
	body, _ := json.Marshal(map[string]any{"chat_id": chatID, "message_id": messageID, "reply_markup": emptyMarkup})
	resp, err := doTelegramPost(api("editMessageReplyMarkup", token), body)
	if resp != nil {
		var result struct {
			Ok bool `json:"ok"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if err == nil && decodeErr == nil && result.Ok {
			return
		}
	}
	if text == "" {
		text = "Closed"
	}
	body, _ = json.Marshal(map[string]any{"chat_id": chatID, "message_id": messageID, "text": text, "reply_markup": emptyMarkup})
	resp, err = doTelegramPost(api("editMessageText", token), body)
	if err != nil {
		logger.Warn("Telegram menu close fallback failed: " + err.Error())
	}
	discardTelegramResponse(resp)
}

func closeCallbackMenu(token string, chatID int64, menuMessageID int, currentText string) {
	if root, ok := menuRootForMessage(chatID, menuMessageID); ok && currentText != root.text {
		// Re-render a named root rather than replaying it. The stored keyboard is a
		// snapshot: it was drawn when this menu was opened, and anything that changed
		// since then is invisible in it. A hub is the clearest case, because every
		// provider row is labelled by state, so a snapshot taken before a provider
		// switch goes on naming the old provider as active.
		if root.name != "" {
			// Keep the body the menu was sent with. Only the keyboard went stale -
			// that is the entire point of re-rendering - and substituting a
			// name-derived label would change the text a close compares against.
			body := root.text
			if text, ok := menuReportText(root.name, chatID); ok {
				body = text
			}
			markup := menus.InlineForChat(root.name, 0, chatID, currentSessionID(chatID))
			editCallback(token, chatID, menuMessageID, body, markup)
			// Record what was just drawn. Returning here without updating left the
			// original snapshot in place, so currentText never equalled root.text
			// and every later Close re-rendered instead of deleting. The message
			// could not be closed at all, and the second tap was refused by
			// Telegram as "message is not modified".
			menuRootByMessage.Store(menuTriggerKey(chatID, menuMessageID), menuRoot{name: root.name, text: body, markup: markup})
			return
		}
		editCallback(token, chatID, menuMessageID, root.text, root.markup)
		menuRootByMessage.Store(menuTriggerKey(chatID, menuMessageID), root)
		return
	}
	triggerMessageID := menuTriggerForMessage(chatID, menuMessageID)
	if err := deleteTelegramMessage(token, chatID, menuMessageID); err != nil {
		logger.Warn("Telegram menu close delete failed: " + err.Error())
	}
	if triggerMessageID > 0 && triggerMessageID != menuMessageID {
		if err := deleteTelegramMessage(token, chatID, triggerMessageID); err != nil {
			logger.Warn("Telegram menu trigger delete failed: " + err.Error())
		}
	}
	menuTriggerByMessage.Delete(menuTriggerKey(chatID, menuMessageID))
	menuRootByMessage.Delete(menuTriggerKey(chatID, menuMessageID))
	menuTriggerPending.Delete(chatID)
}

// callbackQuery is one inline-keyboard tap.
//
// A named type rather than an anonymous struct: Go compares anonymous struct types
// structurally, so a caller has to repeat every field and tag in the exact order to
// satisfy the parameter, which is a trap for any test that wants to drive the
// handler directly.
type callbackQuery struct {
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
}

func handleCallback(token string, q *callbackQuery) {
	answerCallback(token, q.ID)
	if q.Message == nil {
		fmt.Printf("callback missing message id=%s data=%q\n", q.ID, q.Data)
		return
	}
	fmt.Printf("callback handle id=%s data=%q message=%d\n", q.ID, q.Data, q.Message.MessageID)
	chatID := q.Message.Chat.ID
	data := q.Data
	if triggerMessageID := menuTriggerForMessage(chatID, q.Message.MessageID); triggerMessageID > 0 {
		setMenuTriggerPending(chatID, triggerMessageID)
	} else {
		menuTriggerPending.Delete(chatID)
	}
	if data == "close" || data == "close_menu" {
		closeCallbackMenu(token, chatID, q.Message.MessageID, q.Message.Text)
		return
	}
	if data == "noop" {
		return
	}
	if data == "title_model:default" {
		if err := saveTitleModel("gemini-3.5-flash-lite"); err != nil {
			send(token, chatID, "Model save failed: "+err.Error())
			return
		}
		editCallback(token, chatID, q.Message.MessageID, "✅ Title model set to gemini-3.5-flash-lite", menus.InlineForChat("title_settings", 0, chatID, currentSessionID(chatID)))
		return
	}
	if data == "title_model:custom" {
		pendingTitleSetting.Store(chatID, "model")
		markup := map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "Cancel", "callback_data": "title_setting_cancel"}}}}
		sendMenuMarkup(token, chatID, "Send the model ID for automatic session titles.", markup)
		return
	}
	if data == "title_key:set" {
		pendingTitleSetting.Store(chatID, "key")
		markup := map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "Cancel", "callback_data": "title_setting_cancel"}}}}
		sendMenuMarkup(token, chatID, "Send the Gemini API key. It will be stored encrypted and never echoed.", markup)
		return
	}
	if data == "title_setting_cancel" {
		pendingTitleSetting.Delete(chatID)
		editCallback(token, chatID, q.Message.MessageID, "Title setting cancelled", menus.InlineForChat("title_settings", 0, chatID, currentSessionID(chatID)))
		return
	}
	if data == "title_test" {
		title, err := testTitleModel()
		if err != nil {
			send(token, chatID, "Title model test failed: "+err.Error())
			return
		}
		editCallback(token, chatID, q.Message.MessageID, "🧪 Title model works: "+title, menus.InlineForChat("title_settings", 0, chatID, currentSessionID(chatID)))
		return
	}
	if strings.HasPrefix(data, "onboarding:provider:") {
		providerID := strings.TrimPrefix(data, "onboarding:provider:")
		draft := providerDraft{ID: providerID, Stage: "model"}
		switch providerID {
		case "openrouter":
			draft.URL = "https://openrouter.ai/api/v1"
		case "poolside":
			draft.URL = "https://inference.poolside.ai/v1"
		case "hermes":
			draft.URL = ""
		case "codex":
			// Codex needs no URL and no key: the endpoint is fixed and the
			// credential comes from the device login. Selecting it either
			// activates immediately or starts that login, so there is nothing to
			// ask the user for here.
			selectCodexProvider(token, chatID)
			return
		case "custom":
			draft.Stage = "url"
		default:
			send(token, chatID, "Unknown provider choice")
			return
		}
		pendingProvider.Store(chatID, draft)
		if draft.Stage == "url" {
			send(token, chatID, "Send the custom provider base URL.")
		} else {
			send(token, chatID, "Send the model ID for "+providerID+".")
		}
		return
	}
	if data == "onboarding:claim" {
		claimed, err := defaultState.claimAllowedUserID(q.From.ID)
		if err != nil {
			send(token, chatID, "Could not save Telegram allowlist: "+err.Error())
			return
		}
		if !claimed {
			send(token, chatID, "🔒 This bot is already locked to another Telegram account.")
			return
		}
		editCallback(token, chatID, q.Message.MessageID, "🔒 Bot locked to your Telegram account", nil)
		return
	}

	if data == "skill:view" {
		editCallback(token, chatID, q.Message.MessageID, "🧩 Skill proposals", menus.InlineForChat("skill", 0, chatID, currentSessionID(chatID)))
		return
	}
	if strings.HasPrefix(data, "skill:approve:") || strings.HasPrefix(data, "skill:reject:") {
		cfg, err := config.Load()
		if err != nil {
			send(token, chatID, "Config error: "+err.Error())
			return
		}
		store, err := skills.New(cfg.SkillFile)
		if err != nil {
			send(token, chatID, "Skill store error: "+err.Error())
			return
		}
		action := "approve"
		id := strings.TrimPrefix(data, "skill:approve:")
		if strings.HasPrefix(data, "skill:reject:") {
			action = "reject"
			id = strings.TrimPrefix(data, "skill:reject:")
		}
		result := "approved"
		if action == "approve" {
			_, err = store.Approve(id)
		} else {
			result = "rejected"
			err = store.Reject(id)
		}
		if err != nil {
			send(token, chatID, "Skill update failed: "+err.Error())
			return
		}
		editCallback(token, chatID, q.Message.MessageID, "Skill proposal "+result, menus.InlineForChat("skill", 0, chatID, currentSessionID(chatID)))
		return
	}
	if data == "schedule:view" {
		editCallback(token, chatID, q.Message.MessageID, "⏰ Scheduled jobs", menus.InlineForChat("schedule", 0, chatID, currentSessionID(chatID)))
		return
	}
	if strings.HasPrefix(data, "schedule:preset:") {
		interval, err := time.ParseDuration(strings.TrimPrefix(data, "schedule:preset:"))
		if err != nil || interval < time.Minute {
			send(token, chatID, "Invalid schedule interval")
			return
		}
		pendingSchedule.Store(chatID, interval)
		send(token, chatID, "Send the task to run on that schedule as the next message.")
		return
	}
	if strings.HasPrefix(data, "schedule:remove:") {
		cfg, err := config.Load()
		if err == nil {
			store, storeErr := scheduler.New(cfg.SchedulerFile)
			if storeErr == nil {
				_, _ = store.Remove(strings.TrimPrefix(data, "schedule:remove:"))
			}
		}
		editCallback(token, chatID, q.Message.MessageID, "Scheduled job removed", menus.InlineForChat("schedule", 0, chatID, currentSessionID(chatID)))
		return
	}
	if strings.HasPrefix(data, "setsecret:") {
		name := strings.TrimPrefix(data, "setsecret:")
		if name == "" {
			// Free-form: ask for the name first, then the value. Two messages
			// rather than one "name value" line, because parsing a space out of a
			// secret is how a value silently becomes half a secret.
			setPendingSecretName(chatID)
			editCallback(token, chatID, q.Message.MessageID,
				"Send the name for the secret, for example github_token. It is stored "+
					"encrypted and never shown again.", cancelSecretKeyboard())
			return
		}
		editCallback(token, chatID, q.Message.MessageID,
			fmt.Sprintf("Send the value for %s as your next message. It is stored "+
				"encrypted, deleted from this chat, and kept out of the session and "+
				"the model's context.\n\nValid for %d minutes, or press ✕.",
				name, int(pendingSecretTTL.Minutes())), cancelSecretKeyboard())
		setPendingSecret(chatID, name)
		return
	}
	if data == "secret:cancel" {
		clearPendingSecret(chatID)
		clearPendingSecretName(chatID)
		editCallback(token, chatID, q.Message.MessageID, "Cancelled. Nothing stored.",
			menus.InlineForChat("secrets", 0, chatID, currentSessionID(chatID)))
		return
	}
	if strings.HasPrefix(data, "delsecret:") {
		name := strings.TrimPrefix(data, "delsecret:")
		if err := secret.Delete(name); err != nil {
			editCallback(token, chatID, q.Message.MessageID, "Could not delete "+name+": "+err.Error(),
				menus.InlineForChat("secrets", 0, chatID, currentSessionID(chatID)))
			return
		}
		editCallback(token, chatID, q.Message.MessageID, "Deleted "+name,
			menus.InlineForChat("secrets", 0, chatID, currentSessionID(chatID)))
		return
	}
	if strings.HasPrefix(data, "wa:") {
		switch strings.TrimPrefix(data, "wa:") {
		case "link":
			whatsAppLinkNow(token, chatID)
		case "phone":
			// The number arrives as the next plain message, captured by
			// handlePendingInput. Answer the tap with the prompt instead of
			// re-rendering the menu, so there is one thing to reply to.
			pendingWhatsAppPhone.Store(chatID, true)
			send(token, chatID, "Send the WhatsApp phone number in international format, digits only with country code, e.g. 2348012345678.\n\nSend cancel to stop.")
			return
		case "unlink":
			whatsAppUnlink(token, chatID)
		}
		// Re-render from the status the daemon just wrote, so the screen
		// matches the action even though pairing itself is asynchronous.
		editCallback(token, chatID, q.Message.MessageID, whatsAppStatusText(), menus.InlineForChat("whatsapp", 0, chatID, currentSessionID(chatID)))
		return
	}
	if strings.HasPrefix(data, "menu:") {
		name := strings.TrimPrefix(data, "menu:")
		// A menu that reports something has to say it. The generic handler used to
		// send the menu's own name as the whole body, so tapping Status produced the
		// word "status" above a row of buttons while /status printed a full report
		// under an identical keyboard. A title is only a fallback for menus that
		// genuinely are just buttons.
		body := "📋 " + name
		if text, ok := menuReportText(name, chatID); ok {
			body = text
		}
		editCallback(token, chatID, q.Message.MessageID, body, menus.InlineForChat(name, 0, chatID, currentSessionID(chatID)))
		return
	}
	if strings.HasPrefix(data, "select_model:") {
		model := strings.TrimPrefix(data, "select_model:")
		if err := defaultState.setModel(model); err != nil {
			send(token, chatID, "Failed to set model: "+err.Error())
			return
		}
		editCallback(token, chatID, q.Message.MessageID, "✅ Model set to "+model, nil)
		return
	}
	if strings.HasPrefix(data, "select_provider:") {
		provider := strings.TrimPrefix(data, "select_provider:")
		if err := defaultState.setProvider(provider); err != nil {
			send(token, chatID, "Provider save failed: "+err.Error())
			return
		}
		editCallback(token, chatID, q.Message.MessageID, "✅ Provider set to "+provider, menus.InlineForChat("provider", 0, chatID, currentSessionID(chatID)))
		return
	}
	if data == "provider:remove_menu" {
		editCallback(token, chatID, q.Message.MessageID, "🗑 Remove provider", menus.RemoveProviderMenu(defaultState.provider()))
		return
	}
	if strings.HasPrefix(data, "provider:remove:") {
		providerID := strings.TrimPrefix(data, "provider:remove:")
		// Confirmation before deleting: removal switches the active provider to
		// default, which changes where the next task goes.
		editHTML(token, chatID, q.Message.MessageID,
			"Remove provider <b>"+html.EscapeString(providerID)+"</b>?\n\nIf it is the active provider, Ceraa switches to <b>default</b>.",
			map[string]any{"inline_keyboard": [][]map[string]string{{
				{"text": "✅ Remove", "callback_data": "provider:remove_confirm:" + providerID},
				{"text": "✕ Cancel", "callback_data": "menu:provider"},
			}}})
		return
	}
	if strings.HasPrefix(data, "provider:remove_confirm:") {
		providerID := strings.TrimPrefix(data, "provider:remove_confirm:")
		wasActive := defaultState.provider() == providerID
		fromState, fromConfig, stateErr, configErr := removeProviderEverywhere(providerID)
		switch {
		case stateErr != nil && configErr != nil:
			editCallback(token, chatID, q.Message.MessageID, "Could not remove provider: "+stateErr.Error(), nil)
			return
		case stateErr != nil:
			logger.Warn("provider removed from config only, runtime store said: " + stateErr.Error())
		case configErr != nil:
			logger.Warn("provider removed from runtime only, config said: " + configErr.Error())
		}
		logger.Info("provider removed: id=" + providerID + " was_active=" + strconv.FormatBool(wasActive) +
			" from_runtime=" + strconv.FormatBool(fromState) + " from_config=" + strconv.FormatBool(fromConfig))
		note := ""
		if wasActive {
			note = "\n\nIt was active, so Ceraa is now on <b>default</b>."
		}
		if !fromState && !fromConfig {
			note += "\n\nIt was not present in either store, so nothing changed."
		}
		// Return to the provider menu rather than stranding the user on a dead
		// keyboard: the point of removing a provider is usually to pick another.
		editHTML(token, chatID, q.Message.MessageID,
			"🗑 Removed provider <b>"+html.EscapeString(providerID)+"</b>."+note,
			menus.Inline("provider", 0))
		return
	}
	if data == "provider:codex" {
		// Same entry point as the onboarding button, so a configured user reaches
		// Codex from the Provider / Model menu.
		selectCodexProvider(token, chatID)
		return
	}
	// The whole provider family, routed in one place. See provider_route.go: the
	// model list used to have no route at all, so the button silently did nothing.
	// Checked before the cancel handlers and the bare ids below, which it absorbs.
	if providerID, kind, value := routeProviderCallback(data); kind != providerRouteNone {
		switch kind {
		case providerRouteActivate:
			// Same entry point as the onboarding button, so a configured user
			// reaches the provider from the Provider menu.
			switch providerID {
			case "hermes":
				selectHermesProvider(token, chatID)
			case "xai":
				selectXaiProvider(token, chatID)
			case "minimax":
				selectMinimaxProvider(token, chatID)
			case "cline":
				selectClineProvider(token, chatID)
			case "opencode":
				selectOpenCodeProvider(token, chatID)
			case "deepseek":
				selectDeepSeekProvider(token, chatID)
			case "claude":
				selectClaudeProvider(token, chatID)
			case "qoder":
				selectQoderProvider(token, chatID)
			case "gemini":
				selectGeminiProvider(token, chatID)
			case "workbuddy":
				selectBuddyProvider(token, chatID, buddyauth.WorkBuddyProduct())
			}
		case providerRouteModels:
			label := menus.SubscriptionLabel(providerID)
			note := ""
			if active := defaultState.provider(); active != providerID {
				// Naming the provider is the point: this list is deliberately not
				// the active provider's, so the screen has to say whose it is.
				note = "\n\nCeraa is on <b>" + html.EscapeString(active) + "</b>. " +
					"Pick a " + html.EscapeString(label) + " model, then switch to it from Provider."
			}
			editHTML(token, chatID, q.Message.MessageID,
				"🧠 <b>"+html.EscapeString(label)+" models</b>"+note,
				menus.SubscriptionModelMenu(providerID))
		case providerRoutePickModel:
			selectSubscriptionModel(token, chatID, providerID, value)
		case providerRouteRelogin:
			// An explicit re-authentication, distinct from switching to a provider
			// that is already signed in. Reached because the two used to share a
			// callback and the shared one always took the activate branch.
			switch providerID {
			case "minimax":
				startMinimaxLogin(token, chatID)
			case "hermes":
				startHermesLogin(token, chatID)
			case "xai":
				startXaiLogin(token, chatID)
			case "cline":
				startClineLogin(token, chatID)
			case "deepseek":
				startDeepSeekLogin(token, chatID)
			case "qoder":
				startQoderLogin(token, chatID)
			case "gemini":
				startGeminiLogin(token, chatID)
			case "workbuddy":
				startBuddyLogin(token, chatID, buddyauth.WorkBuddyProduct())
			case "opencode":
				// Re-entering the key. There is no token to refresh, so this is
				// the same onboarding flow the first key went through.
				startOpenCodeOnboarding(token, chatID)
			}
		}
		return
	}
	if data == "provider:custom" {
		pendingProvider.Store(chatID, providerDraft{ID: "custom", Stage: "url"})
		send(token, chatID, "Send the provider base URL for the custom profile.")
		return
	}
	if data == "model_catalog_search" {
		pendingModelSearch.Store(chatID, true)
		send(token, chatID, "Send words to search for in the model catalog.")
		return
	}
	if data == "provider:model_manual" {
		cfg, err := config.Load()
		if err != nil {
			send(token, chatID, "Config error: "+err.Error())
			return
		}
		profile := activeProviderConfig(cfg, defaultState.provider())
		pendingProvider.Store(chatID, providerDraft{URL: profile.URL, ID: defaultState.provider(), Stage: "model"})
		send(token, chatID, "Send the model ID to activate.")
		return
	}
	if strings.HasPrefix(data, "model:") {
		index, err := strconv.Atoi(strings.TrimPrefix(data, "model:"))
		if err != nil {
			return
		}
		metadata, ok := menus.ModelChoice(index)
		if !ok {
			send(token, chatID, "Model selection expired. Refresh the catalog.")
			return
		}
		cfg, err := config.Load()
		if err != nil {
			send(token, chatID, "Config error: "+err.Error())
			return
		}
		current := activeProviderConfig(cfg, defaultState.provider())
		profile := config.ProviderProfile{URL: current.URL, Model: metadata.ID, ContextLimit: current.ContextLimit, MaxOutputTokens: current.MaxOutputTokens, PromptCacheMode: current.PromptCacheMode, PromptCacheTTL: current.PromptCacheTTL, Subscription: current.Subscription, HermesProvider: current.HermesProvider}
		if err := defaultState.setProviderProfile(defaultState.provider(), profile); err != nil {
			send(token, chatID, "Model save failed: "+err.Error())
			return
		}
		if err := defaultState.setProvider(defaultState.provider()); err != nil {
			send(token, chatID, "Provider activation failed: "+err.Error())
			return
		}
		editCallback(token, chatID, q.Message.MessageID, "✅ Model set to "+metadata.ID, menus.InlineForChat("provider", 0, chatID, currentSessionID(chatID)))
		return
	}
	if strings.HasPrefix(data, "select_agent:") {
		agent := strings.TrimPrefix(data, "select_agent:")
		if err := defaultState.setAgent(agent); err != nil {
			send(token, chatID, "Failed to set agent: "+err.Error())
			return
		}
		editCallback(token, chatID, q.Message.MessageID, "✅ Agent set to "+agent, nil)
		return
	}
	if strings.HasPrefix(data, "select_reasoning:") {
		level := strings.TrimPrefix(data, "select_reasoning:")
		// "default" clears the runtime override so the provider decides.
		if level == "default" {
			level = ""
		}
		if err := defaultState.setReasoning(level); err != nil {
			send(token, chatID, "Failed to set thinking: "+err.Error())
			return
		}
		editCallback(token, chatID, q.Message.MessageID, "🧠 Thinking set to "+thinkingLabel(level), nil)
		return
	}
	if strings.HasPrefix(data, "session_rename:") {
		id := strings.TrimPrefix(data, "session_rename:")
		pendingSessionAction.Store(chatID, sessionAction{ID: id, Action: "rename"})
		markup := map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "Cancel", "callback_data": "session_action_cancel"}}}}
		sendMenuMarkup(token, chatID, "Send the new session title.", markup)
		return
	}
	if data == "session_action_cancel" {
		pendingSessionAction.Delete(chatID)
		editCallback(token, chatID, q.Message.MessageID, "Rename cancelled", menus.InlineForChat("session", 0, chatID, currentSessionID(chatID)))
		return
	}
	if strings.HasPrefix(data, "session_regenerate:") {
		id := strings.TrimPrefix(data, "session_regenerate:")
		title, err := generateSessionTitle(id, true)
		if err != nil {
			send(token, chatID, "Title regeneration failed: "+err.Error())
			return
		}
		editCallback(token, chatID, q.Message.MessageID, "🔁 Session title: "+title, menus.InlineForChat("session", 0, chatID, currentSessionID(chatID)))
		return
	}
	if strings.HasPrefix(data, "session_delete:") {
		id := strings.TrimPrefix(data, "session_delete:")
		store, err := configuredStore()
		if err != nil {
			send(token, chatID, "Session store error: "+err.Error())
			return
		}
		sess, err := store.Load(id)
		if err != nil || sess.ID != id {
			send(token, chatID, "Session not found")
			return
		}
		markup := map[string]any{"inline_keyboard": [][]map[string]string{
			{{"text": "🗑 Delete " + sess.DisplayTitle(), "callback_data": "session_delete_confirm:" + id}},
			{{"text": "Cancel", "callback_data": "session_delete_cancel"}},
		}}
		editCallback(token, chatID, q.Message.MessageID, "Delete this session? This cannot be undone.", markup)
		return
	}
	if data == "session_delete_cancel" {
		editCallback(token, chatID, q.Message.MessageID, "Delete cancelled", menus.InlineForChat("session", 0, chatID, currentSessionID(chatID)))
		return
	}
	if strings.HasPrefix(data, "session_delete_confirm:") {
		id := strings.TrimPrefix(data, "session_delete_confirm:")
		if err := deleteSession(id); err != nil {
			send(token, chatID, "Delete failed: "+err.Error())
			return
		}
		if currentSessionID(chatID) == id {
			startNewSession(token, chatID)
			return
		}
		editCallback(token, chatID, q.Message.MessageID, "🗑 Session deleted", menus.InlineForChat("session", 0, chatID, currentSessionID(chatID)))
		return
	}
	if strings.HasPrefix(data, "attach_session:") || strings.HasPrefix(data, "open_session:") {
		id := strings.TrimPrefix(data, "attach_session:")
		if strings.HasPrefix(data, "open_session:") {
			id = strings.TrimPrefix(data, "open_session:")
		}
		store, err := configuredStore()
		if err != nil {
			send(token, chatID, "Session store error: "+err.Error())
			return
		}
		sess, err := store.Load(id)
		if err != nil || sess.ID != id || id == "inbox" {
			send(token, chatID, "Session not found: "+id)
			return
		}
		if err := defaultState.setSession(chatID, id); err != nil {
			send(token, chatID, "Failed to save active session: "+err.Error())
			return
		}
		editCallback(token, chatID, q.Message.MessageID, fmt.Sprintf("✅ Opened %s (%d messages)", sess.DisplayTitle(), len(sess.Messages)), menus.InlineForChat("session", 0, chatID, id))
		return
	}
	if data == "new_session" {
		startNewSession(token, chatID)
		return
	}
	if strings.HasPrefix(data, "project:") {
		path := strings.TrimPrefix(data, "project:")
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			send(token, chatID, "Project directory not found: "+path)
			return
		}
		if err := defaultState.setProject(chatID, path); err != nil {
			send(token, chatID, "Failed to set project: "+err.Error())
			return
		}
		editCallback(token, chatID, q.Message.MessageID, "📁 Project set to "+defaultState.project(chatID), nil)
		return
	}
	if strings.HasPrefix(data, "browse:") {
		handleBrowseCallback(token, chatID, q.Message.MessageID, strings.TrimPrefix(data, "browse:"))
		return
	}
	if strings.HasPrefix(data, "show_messages:") {
		n, _ := strconv.Atoi(strings.TrimPrefix(data, "show_messages:"))
		store, err := configuredStore()
		if err != nil {
			send(token, chatID, "Session store error: "+err.Error())
			return
		}
		sess, err := store.Load(currentSessionID(chatID))
		if err != nil {
			send(token, chatID, "No messages")
			return
		}
		start := len(sess.Messages) - n
		if start < 0 {
			start = 0
		}
		var lines []string
		for _, message := range sess.Messages[start:] {
			preview := message.Content
			if preview == "" {
				preview = message.ToolResult
			}
			if len(preview) > 120 {
				preview = preview[:120] + "..."
			}
			lines = append(lines, string(message.Role)+": "+preview)
		}
		send(token, chatID, strings.Join(lines, "\n"))
		return
	}
	if strings.HasPrefix(data, "tool:") {
		name := strings.TrimPrefix(data, "tool:")
		if registered, ok := tool.DefaultRegistry().Get(name); ok {
			send(token, chatID, fmt.Sprintf("🔧 `%s`\n%s\nSchema: `%s`", name, registered.Description(), mustJSON(registered.Schema())))
			return
		}
		send(token, chatID, "Tool not found: "+name)
		return
	}
	if strings.HasPrefix(data, "todo:") {
		handleTodoCallback(token, chatID, q.Message.MessageID, strings.TrimPrefix(data, "todo:"))
		return
	}
	if strings.HasPrefix(data, "permission:") {
		handlePermissionMenuCallback(token, chatID, strings.TrimPrefix(data, "permission:"))
		return
	}
	if strings.HasPrefix(data, "perm:") {
		// perm:<token>:<code>. The request id and tool name live behind the token;
		// they were inlined here and overran Telegram's 64 byte limit, which cost
		// the whole menu.
		parts := strings.Split(data, ":")
		if len(parts) != 3 {
			send(token, chatID, "Invalid permission response")
			return
		}
		ref, code := parts[1], parts[2]
		decision := ""
		switch code {
		case "a":
			decision = "allowOnce"
		case "s":
			decision = "allowAlways"
		case "d":
			decision = "deny"
		default:
			send(token, chatID, "Invalid permission response")
			return
		}
		pending, ok := peekCallbackToken(ref)
		if !ok {
			send(token, chatID, "That permission request has expired. Send the message again.")
			return
		}
		store, err := configuredStore()
		if err != nil || !answerTelegramPermission(store, pending.SessionID, pending.RequestID, decision) {
			dropCallbackToken(ref)
			send(token, chatID, "Permission request expired or was already answered")
			return
		}
		dropCallbackToken(ref)
		if decision == "allowAlways" && pending.Tool != "" {
			permission.Default.AllowAlways(pending.Tool)
			_ = defaultState.setAlwaysAllow(pending.Tool, true)
		}
		editCallback(token, chatID, q.Message.MessageID, "Permission "+decision+" recorded", nil)
		return
	}
	if strings.HasPrefix(data, "qtype:") {
		// qtype:<token>, same scheme as qans. The token has to be resolved here
		// rather than passed through: what expectAnswer needs is the real request
		// id, and handing it the token would leave the typed reply matching nothing.
		ref := strings.TrimPrefix(data, "qtype:")
		pending, ok := peekCallbackToken(ref)
		if !ok {
			send(token, chatID, "That question has expired. Ask again.")
			return
		}
		dropCallbackToken(ref)
		// A question with no options is already free-form, so the prompt is just a
		// reminder; one with options now allows an answer outside them.
		defaultState.expectAnswer(pending.RequestID, chatID)
		editCallback(token, chatID, q.Message.MessageID, "✍️ Reply with your answer.", nil)
		return
	}
	if strings.HasPrefix(data, "qans:") {
		// qans:<token>. The answer text is behind the token: it used to be inlined
		// and a single sentence overran the 64 byte limit, so questions with real
		// options could not be rendered at all.
		ref := strings.TrimPrefix(data, "qans:")
		pending, ok := takeCallbackToken(ref)
		if !ok {
			send(token, chatID, "That question has expired. Ask again.")
			return
		}
		store, err := configuredStore()
		if err != nil || !answerTelegramQuestion(store, pending.SessionID, pending.RequestID, pending.Answer) {
			send(token, chatID, "Question already answered or expired")
			return
		}
		editCallback(token, chatID, q.Message.MessageID, "✓ Answered: "+pending.Answer, nil)
		return
	}
	if strings.HasPrefix(data, "run:stop") {
		// The Stop button has been removed; /stop is the way to cancel. Buttons
		// from before the removal are still sitting in chats, so the tap is answered
		// with the replacement rather than ignored.
		editHTML(token, chatID, q.Message.MessageID,
			"That button is gone. Send /stop to cancel a running task.", nil)
		return
	}
	if data == "context:clear" {
		// The clear-context action and its store method have been removed. The
		// callback is still answered because keyboards already sitting in a chat
		// carry this button, and silently ignoring the tap would look like a hung
		// bot rather than a removed feature.
		send(token, chatID, "Clear context has been removed. Use 🗜️ Compact now to shrink the context without discarding history.")
		return
	}
	if strings.HasPrefix(data, "context:") {
		handleContextCommand(token, chatID)
		return
	}
	if data == "compaction:auto_on" {
		handleCompaction(token, chatID, "/compaction auto on")
		return
	}
	if data == "compaction:auto_off" {
		handleCompaction(token, chatID, "/compaction auto off")
		return
	}
	if strings.HasPrefix(data, "compact:") || strings.HasPrefix(data, "compaction:") {
		handleCompaction(token, chatID, "/compaction now")
		return
	}
	if data == "snapshots:list" {
		handleSnapshotsCommand(token, chatID)
		return
	}
	if data == "revert:prompt" {
		send(token, chatID, "Send /revert <path> to restore the latest snapshot for that file.")
		return
	}
	if strings.HasPrefix(data, "run_command:") {
		executeCallbackCommand(token, chatID, strings.TrimPrefix(data, "run_command:"))
		return
	}
	if data == "worktree:list" {
		output, err := exec.Command("git", "worktree", "list").CombinedOutput()
		if err != nil {
			send(token, chatID, "Worktree lookup failed: "+err.Error())
			return
		}
		send(token, chatID, string(output))
		return
	}
	if strings.HasPrefix(data, "hermes:check:") {
		flowID := strings.TrimPrefix(data, "hermes:check:")
		flow, err := hermesAuthMgr.Check(flowID)
		if err != nil {
			editCallback(token, chatID, q.Message.MessageID, "❌ "+err.Error(), nil)
			return
		}
		status, _, _ := flow.StatusSnapshot()
		if status == "succeeded" {
			// The watcher decides this normally, and it refuses an activation
			// whose switch was superseded. Pressing Check reached the activation
			// without passing through that guard, so a user who signed in, switched
			// to another provider while waiting, and then tapped Check was switched
			// back by a button labelled with a flow id.
			//
			// An unowned flow is reported and not switched. The owner is cleared
			// once the watcher has delivered, so a Check after that finds nothing
			// and correctly does nothing: the decision was already made, in the
			// one place that makes it.
			if owner, owned := hermesFlowOwnerOf(flowID); owned &&
				flowActivationStillWanted("Hermes", owner.activate, owner.activeAtStart) {
				activateHermesProvider(token, chatID, flowID)
				return
			}
			editCallback(token, chatID, q.Message.MessageID,
				"✅ Hermes sign-in completed. The Nous Portal credential is stored and ready.",
				hermesStatusKeyboard(false))
			return
		}
		editCallback(token, chatID, q.Message.MessageID, flow.ResultMessage(), nil)
		return
	}
	if strings.HasPrefix(data, "hermes:cancel:") {
		flowID := strings.TrimPrefix(data, "hermes:cancel:")
		if err := hermesAuthMgr.Cancel(flowID); err != nil {
			editCallback(token, chatID, q.Message.MessageID, "❌ "+err.Error(), nil)
			return
		}
		editCallback(token, chatID, q.Message.MessageID, "Hermes login cancelled.", nil)
		return
	}
	if data == "codex:login" {
		// Start a login from a button. Records the owner so the watcher reports
		// the outcome to this chat whether or not anyone presses Check.
		handleCodexLogin(token, chatID)
		return
	}
	if data == "codex:activate" {
		activateCodexProvider(token, chatID)
		return
	}
	if data == "codex:status" {
		handleCodexStatus(token, chatID)
		return
	}
	if strings.HasPrefix(data, "codex:check:") {
		flowID := strings.TrimPrefix(data, "codex:check:")
		flow, err := authMgr.Check(flowID)
		if err != nil {
			// A finished flow is not in the registry, which is expected rather than
			// a failure. By now the completion callback has reported the outcome, so
			// say what actually holds and offer the button that moves things on.
			if codexauth.Authenticated() {
				editCallback(token, chatID, q.Message.MessageID,
					"✅ This sign-in already completed. The outcome was reported when it finished.",
					codexStatusKeyboard(true, defaultState.provider() == llm.CodexSubscription))
				return
			}
			editCallback(token, chatID, q.Message.MessageID,
				"This sign-in is no longer active. Start a new one below.",
				map[string]any{"inline_keyboard": [][]map[string]string{
					{{"text": "🔁 Start sign-in again", "callback_data": "codex:login"}},
				}})
			return
		}
		state := flow
		// Completion is reported by the manager's callback, so there is nothing to
		// activate here. A tap while the login is still in flight just refreshes
		// the message.
		// Keep the flow's own keyboard, which now offers a retry or a switch
		// rather than being emptied. An emptied keyboard is a dead message.
		markup, kbErr := flow.InlineKeyboardJSON()
		var kb map[string]any
		if kbErr != nil || json.Unmarshal(markup, &kb) != nil {
			kb = codexStatusKeyboard(codexauth.Authenticated(), defaultState.provider() == llm.CodexSubscription)
		}
		editCallback(token, chatID, q.Message.MessageID, state.ResultMessage(), kb)
		return
	}
	if strings.HasPrefix(data, "codex:cancel:") {
		flowID := strings.TrimPrefix(data, "codex:cancel:")
		authMgr.Cancel(flowID)
		clearCodexFlow(flowID)
		// Leave a way back. Cancelling used to remove every button.
		sendHTML(token, chatID, "Login cancelled.",
			map[string]any{"inline_keyboard": [][]map[string]string{
				{{"text": "🔁 Start sign-in again", "callback_data": "codex:login"}},
			}})
		return
	}
}

// menuReportText is the body for menus that show data rather than only buttons.
//
// Deliberately a lookup rather than a general mechanism: most menus are action
// lists and their name is a fine label, so the ones that report are named here.
func menuReportText(name string, chatID int64) (string, bool) {
	switch name {
	case "status", "context":
		text, err := statusReport(chatID)
		if err != nil {
			return "Status unavailable: " + err.Error(), true
		}
		return text, true
	case "whatsapp":
		return whatsAppStatusText(), true
	}
	return "", false
}

// removeProviderEverywhere deletes a provider profile from every store the menus
// read, and reports which ones held it.
//
// Both, because the menus merge config.json with bridge-state.json. Clearing only
// the runtime store left a config.json copy, so the provider was still listed the
// instant it was removed. That is the whole defect, so it lives in one function
// rather than being spread across a callback and a helper.
func removeProviderEverywhere(id string) (fromState, fromConfig bool, stateErr, configErr error) {
	fromState, stateErr = defaultState.removeProviderProfile(id)
	fromConfig, configErr = removeProviderFromConfig(id)
	return fromState, fromConfig, stateErr, configErr
}

// removeProviderFromConfig deletes a provider profile from config.json, reporting
// whether one was there. A missing profile is not an error: the caller removes from
// both stores and either succeeding is enough.
func removeProviderFromConfig(id string) (bool, error) {
	cfg, err := config.Load()
	if err != nil {
		return false, err
	}
	if _, ok := cfg.ProviderProfiles[id]; !ok {
		return false, nil
	}
	if err := cfg.RemoveProviderProfile(id); err != nil {
		return false, err
	}
	return true, nil
}

func handleBrowseCallback(token string, chatID int64, messageID int, path string) {
	root := defaultState.project(chatID)
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		send(token, chatID, "Invalid path")
		return
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		send(token, chatID, "Path is outside the active project")
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		send(token, chatID, "Path not found: "+rel)
		return
	}
	if info.IsDir() {
		editCallback(token, chatID, messageID, "📁 "+rel, menus.InlinePath(abs))
		return
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		send(token, chatID, "Read failed: "+err.Error())
		return
	}
	content := string(data)
	if len(content) > 3500 {
		content = content[:3500] + "...[truncated]"
	}
	send(token, chatID, "```\n"+content+"\n```")
}

func handleTodoCallback(token string, chatID int64, messageID int, value string) {
	parts := strings.SplitN(value, ":", 2)
	if len(parts) != 2 {
		return
	}
	index, err := strconv.Atoi(parts[0])
	if err != nil {
		return
	}
	cfg, err := config.Load()
	if err != nil {
		send(token, chatID, "Config error: "+err.Error())
		return
	}
	store, err := todo.New(cfg.SessionFile)
	if err != nil {
		send(token, chatID, "Todo store error: "+err.Error())
		return
	}
	items, err := store.Load(currentSessionID(chatID))
	if err != nil || index < 0 || index >= len(items) {
		send(token, chatID, "Todo item not found")
		return
	}
	if items[index].Status == "completed" {
		items[index].Status = "pending"
	} else {
		items[index].Status = "completed"
	}
	if err := store.Save(currentSessionID(chatID), items); err != nil {
		send(token, chatID, "Todo update failed: "+err.Error())
		return
	}
	editCallback(token, chatID, messageID, "✅ Todo updated", menus.InlineForChat("todos", 0, chatID, currentSessionID(chatID)))
}

func handlePermissionMenuCallback(token string, chatID int64, action string) {
	switch action {
	case "policy":
		send(token, chatID, "🔒 Write, delete, move, copy, and risky shell operations require approval. Read/search tools run without approval.")
	case "allow_read":
		for _, name := range []string{"read", "glob", "grep"} {
			permission.Default.AllowAlways(name)
			_ = defaultState.setAlwaysAllow(name, true)
		}
		send(token, chatID, "✅ Read/search tools will be allowed without prompts.")
	case "reset":
		for _, name := range []string{"read", "glob", "grep", "write", "bash"} {
			permission.Default.ClearAlways(name)
			_ = defaultState.setAlwaysAllow(name, false)
		}
		send(token, chatID, "Permission policy reset.")
	default:
		send(token, chatID, "Unknown permission action")
	}
}

func executeCallbackCommand(token string, chatID int64, command string) {
	command = strings.TrimSpace(command)
	switch command {
	case "/status":
		handleStatus(token, chatID)
	case "/settings":
		handleSettings(token, chatID)
	case "/sessions":
		handleSessions(token, chatID)
	case "/messages":
		handleMessages(token, chatID)
	case "/tools":
		handleToolsCommand(token, chatID)
	case "/todos":
		handleTodosCommand(token, chatID)
	case "/context":
		handleContextCommand(token, chatID)
	case "/model":
		handleModelCommand(token, chatID, command)
	case "/agent":
		handleAgentCommand(token, chatID, command)
	case "/project":
		handleProjectCommand(token, chatID, command)
	case "/permission":
		handlePermissionCommand(token, chatID)
	case "/compaction", "/compaction_level":
		handleCompaction(token, chatID, command)
	case "/snapshots":
		handleSnapshotsCommand(token, chatID)
	case "/revert":
		handleRevertCommand(token, chatID, command)
	case "/file_browser":
		sendMenuMarkup(token, chatID, "📂 Workspace files", menus.InlineForChat("file_browser", 0, chatID, currentSessionID(chatID)))
	case "/skills_catalog":
		handleCatalogCommand(token, chatID, command)
	case "/help":
		handleFullHelp(token, chatID)
	case "/new":
		startNewSession(token, chatID)
	case "/detach":
		detachedChats.Store(chatID, true)
		send(token, chatID, "🔌 Detached. Send /attach to reattach.")
	case "/attach":
		detachedChats.Delete(chatID)
		send(token, chatID, "🔗 Attached to session "+currentSessionID(chatID))
	case "/codex-login", "/codex_login":
		handleCodexLogin(token, chatID)
	case "/codex-status", "/codex_status":
		handleCodexStatus(token, chatID)
	case "/hub", "/menu":
		sendHub(token, chatID)
	default:
		send(token, chatID, "Use that command with an argument, for example /"+strings.TrimPrefix(command, "/")+" value")
	}
}

func mustJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

// thinkingLabel names a reasoning level for display, distinguishing the provider
// default from an explicit choice.
func thinkingLabel(level string) string {
	if strings.TrimSpace(level) == "" {
		return "default"
	}
	return level
}
