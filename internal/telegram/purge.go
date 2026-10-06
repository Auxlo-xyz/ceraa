package telegram

import (
	"fmt"
	"time"

	"ceraa/internal/logger"
	"ceraa/internal/miniapp"
)

// purgeNotice is how long the purge path waits for a fresh hostname before
// giving up and reporting the failure.
//
// Long enough for the announcement to arrive on a connection that has to be
// built from scratch, short enough that a wedged tunnel is reported rather than
// left to be discovered by tapping a button that was just made worse.
const purgeNotice = 20 * time.Second

// handlePurge retires the current tunnel and opens a new one.
//
// It exists because a quick tunnel's hostname is assigned per client process
// and dies with it, while the button in Telegram is cached by the client. After
// a restart the server has correctly republished the button, but a phone that
// has not refetched it keeps sending taps to a hostname that no longer resolves
// - Cloudflare's 1033, which reads as Ceraa being down rather than as a cache.
//
// No bot API call can evict that cache. What can be done is never rely on the
// cached one: the purge closes the tunnel, mints a fresh hostname, republishes
// the button, and sends the new address as an inline web_app button in the chat.
// Telegram resolves an inline button's URL when the tap is made, not from a
// cached menu-button value, so that link opens the live surface even while the
// menu button is still stale. It is the difference between a button that is
// eventually right and a link that is right every time it is used.
// republishMiniAppAddress moves the button and posts a tap-time link, with no
// chat message saying a repair happened.
//
// This is the automatic half. A quick tunnel's hostname changes on every boot
// and whenever the client dies, but Telegram caches the button server-side per
// client, so a stale phone keeps opening a dead URL for a while after the server
// has already published the new one. There is no API to evict that cache. What
// there is to do is stop depending on it: the button is still moved, and a fresh
// inline link is pushed to the chat, which is resolved when tapped. The user is
// never asked to run anything, and never told about the churn unless they look
// at the log.
// republishMiniAppAddress moves the button to a new address, quietly.
//
// This is the automatic half. A quick tunnel's hostname changes on every boot
// and whenever the client dies, but Telegram caches the button per client, so a
// phone can keep opening a dead URL for a while after the server has published
// the new one. There is no API to evict that cache and no way to shorten it from
// here, so the only honest thing left is to keep the button correct and stay
// quiet.
//
// It stays quiet on purpose. Announcing each move put a message in the chat on
// every restart and every repaired tunnel, and a chat that gets told about a
// URL it did not ask for reads as noise: the owner asked for a restart, not for
// a second link to compare against the first. The button is the surface the
// address belongs on, so that is the only surface it moves.
func republishMiniAppAddress(token string, srv *miniapp.Server, origin string) {
	if err := setChatMenuButton(token, "🌐 Web", origin); err != nil {
		logger.Warn("Mini App republish: menu button not updated: " + err.Error())
		return
	}
	noteMiniAppPublished(origin)
	logger.Info("Mini App republished at " + origin)
}

// handlePurge forces a brand-new tunnel right now.
//
// It is the manual half of the same idea republishMiniAppAddress handles
// automatically: the button is stubborn, so this gives a fresh tap-time link on
// demand rather than waiting for a restart to mint one.
func handlePurge(token string, chatID int64, start func() (*miniapp.Server, error)) {
	server, err := start()
	if err != nil {
		send(token, chatID, "Mini App unavailable: "+err.Error())
		return
	}
	send(token, chatID, "Purging the tunnel and minting a fresh address...")
	previous := server.CloseTunnel()
	if previous != "" {
		logger.Info("Mini App purged tunnel at " + previous)
	}
	// The wait is bounded here rather than left to the caller's goroutine, so
	// the chat is always told one way or the other. A silent purge is the thing
	// this command exists to replace.
	result := make(chan string, 1)
	go func() {
		address, startErr := server.StartTunnel()
		if startErr != nil {
			result <- ""
			logger.Warn("Mini App tunnel unavailable after purge: " + startErr.Error())
			return
		}
		result <- address
	}()
	select {
	case address := <-result:
		if address == "" {
			send(token, chatID, "No public address. The tunnel client could not start - see the log.")
			return
		}
		publishMiniAppAddress(token, chatID, address)
		// The inline button, which is the part that survives a stale menu
		// button. Sent as its own message so it is the thing a thumb lands on.
		sendMessageWithMarkup(token, chatID, "Fresh address - tap to open:", purgeKeyboard(address))
		logger.Info("Mini App purged and republished at " + address)
	case <-time.After(purgeNotice):
		send(token, chatID, fmt.Sprintf("Tunnel did not answer within %s. Try again in a moment.", purgeNotice))
	}
}

// purgeKeyboard is the inline web_app button carrying a fresh address.
func purgeKeyboard(address string) map[string]any {
	return map[string]any{
		"inline_keyboard": [][]map[string]any{{
			{
				"text": "🌐 Open Ceraa",
				"web_app": map[string]any{
					"url": address,
				},
			},
		}},
	}
}
