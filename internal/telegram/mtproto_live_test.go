package telegram

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
)

// TestMTProtoLiveConnect is skipped unless CERAA_MTP_LIVE=1. It checks that
// the baked application credential is accepted by Telegram and that the bot
// can authorize with it, without touching any file download.
func TestMTProtoLiveConnect(t *testing.T) {
	if os.Getenv("CERAA_MTP_LIVE") != "1" {
		t.Skip("live MTProto check needs CERAA_MTP_LIVE=1")
	}
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		t.Fatal("TELEGRAM_BOT_TOKEN is not set")
	}
	appID, appHash, ok := mtprotoCredentials()
	if !ok {
		t.Fatal("no application credential baked or env-provided")
	}
	dir := t.TempDir()
	client := telegram.NewClient(appID, appHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: filepath.Join(dir, "probe.session")},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	err := client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("auth status: %w", err)
		}
		if !status.Authorized {
			if _, err := client.Auth().Bot(ctx, token); err != nil {
				return fmt.Errorf("bot authorization: %w", err)
			}
		}
		t.Logf("authorized as bot, credential %d accepted", appID)
		return nil
	})
	if err != nil {
		t.Fatalf("live connect failed: %v", err)
	}
}
