package telegram

import (
	"os"
	"strconv"
)

// Baked MTProto application credentials.
//
// Empty in the source tree on purpose: the values are injected at build time
// with -ldflags "-X ceraa/internal/telegram.bakedAPIID=<id> -X
// ceraa/internal/telegram.bakedAPIHash=<hash>", sourced from the deploy box's
// env file, so the public mirror never carries them. Extractability from a
// shipped binary is accepted, the same way it is for every Telegram client:
// the app id identifies the application, not the operator, and the blast
// radius of abuse is app-level throttling, not account bans, because each box
// still authenticates with its own bot token and its own MTProto auth key.
var (
	bakedAPIID   string
	bakedAPIHash string
)

// mtprotoCredentials resolves the application credential for the MTProto
// download path. An environment override wins over the baked-in values, so a
// box can run with its own app credential without a rebuild; with neither
// present the path is unavailable and downloads fall back to the Bot API.
// Never lift a leaked or sample credential in here: Telegram limits those
// server side (API_ID_PUBLISHED_FLOOD) and flags traffic against them.
func mtprotoCredentials() (int, string, bool) {
	id, hash := bakedAPIID, bakedAPIHash
	if v := os.Getenv("CERAA_TG_API_ID"); v != "" {
		id = v
	}
	if v := os.Getenv("CERAA_TG_API_HASH"); v != "" {
		hash = v
	}
	n, err := strconv.Atoi(id)
	if err != nil || n <= 0 || hash == "" {
		return 0, "", false
	}
	return n, hash, true
}
