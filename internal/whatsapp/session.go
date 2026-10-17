package whatsapp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"

	_ "modernc.org/sqlite"
)

// openSession loads or creates the persisted device state and returns a
// client for it. The store upgrades itself on open. Auto reconnect keeps
// its default (on); nothing here changes it.
func openSession(ctx context.Context, dbPath string) (*whatsmeow.Client, *sqlstore.Container, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, nil, fmt.Errorf("whatsapp: create session dir: %w", err)
	}
	dsn := "file:" + dbPath + "?_pragma=foreign_keys(1)"
	container, err := sqlstore.New(ctx, "sqlite", dsn, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("whatsapp: open session store: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		_ = container.Close()
		return nil, nil, fmt.Errorf("whatsapp: load device: %w", err)
	}
	return whatsmeow.NewClient(device, nil), container, nil
}
