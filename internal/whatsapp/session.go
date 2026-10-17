package whatsapp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"

	_ "modernc.org/sqlite"
)

// deviceOSName is what the phone lists under Linked devices. The library
// defaults DeviceProps.Os to "whatsmeow", so a link made without this reads
// as the library rather than as Ceraa.
const deviceOSName = "Ceraa"

// openSession loads or creates the persisted device state and returns a
// client for it. The store upgrades itself on open. Auto reconnect keeps
// its default (on); nothing here changes it.
func openSession(ctx context.Context, dbPath string) (*whatsmeow.Client, *sqlstore.Container, error) {
	// The linked-device label is read from DeviceProps, which is a
	// package-level default set before pairing, so a fresh link is
	// registered as Ceraa instead of the library's own name.
	store.SetOSInfo(deviceOSName, [3]uint32{1, 0, 0})
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
