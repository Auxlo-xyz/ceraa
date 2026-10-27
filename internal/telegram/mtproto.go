package telegram

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gotd/td/fileid"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"

	"ceraa/internal/paths"
)

// errMTProtoUnavailable says the MTProto path has no application credential to
// run with, so the caller should keep the plain Bot API behaviour instead of
// reporting a download failure.
var errMTProtoUnavailable = errors.New("mtproto: no application credential")

// mtprotoTimeout bounds one MTProto download. A multi-gigabyte transfer can
// legitimately run long, but an unkillable download is worse than a slow one
// because the update loop moves on without it.
const mtprotoTimeout = 30 * time.Minute

// mtprotoDownload downloads a Telegram file straight over MTProto, using the
// baked-in application credential and the bot token as the session. This is
// the path the hosted Bot API cannot take: getFile refuses anything past the
// 20 MB bot cap, while the raw upload.getFile the Bot API wraps has no such
// limit.
//
// The session file lives in the data directory and is derived from the bot
// token on first use, so a fresh box brings up large-file downloads with zero
// configuration: the operator sets nothing and the sender sets nothing. The
// session is the bot's, not a user account's.
//
// name, when the update carried a filename, becomes the saved file's name so
// the extension survives; without one the file lands under a hash-derived
// handle, the same trade the Bot API path makes with file_NNN.
func mtprotoDownload(token, fileID, name string) (string, error) {
	appID, appHash, ok := mtprotoCredentials()
	if !ok {
		return "", errMTProtoUnavailable
	}
	fid, err := fileid.DecodeFileID(fileID)
	if err != nil {
		return "", fmt.Errorf("file id did not decode: %w", err)
	}
	loc, ok := fid.AsInputFileLocation()
	if !ok {
		return "", errors.New("file id does not name a downloadable location")
	}

	dir := paths.FilesDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := mtprotoDestPath(dir, fileID, name)

	ctx, cancel := context.WithTimeout(context.Background(), mtprotoTimeout)
	defer cancel()

	client := telegram.NewClient(appID, appHash, telegram.Options{
		// The session persists, so the bot authorization handshake runs once
		// per box and every later download reuses the stored auth key.
		SessionStorage: &session.FileStorage{Path: filepath.Join(paths.DataDir(), "mtproto.session")},
	})
	err = client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("auth status: %w", err)
		}
		if !status.Authorized {
			if _, err := client.Auth().Bot(ctx, token); err != nil {
				return fmt.Errorf("bot authorization: %w", err)
			}
		}
		// ToPath also reports the detected storage type; only success
		// matters here. The downloader talks to the raw API handle, not
		// the client wrapper, which lacks the CDN hash methods.
		if _, err := client.Downloader().Download(client.API(), loc).ToPath(ctx, dst); err != nil {
			return fmt.Errorf("mtproto download: %w", err)
		}
		return nil
	})
	if err != nil {
		// A half-written file must not look like a download that succeeded:
		// the caller treats a returned path as done.
		_ = os.Remove(dst)
		return "", err
	}
	return dst, nil
}

// mtprotoDestPath picks where an MTProto download lands in the files
// directory: the sender's name when one is known, else a hash-derived handle
// so two files never overwrite each other silently. An already-taken name
// gains a timestamp rather than a counter, matching the never-overwrite rule
// saveWithSenderName enforces on the Bot API path.
func mtprotoDestPath(dir, fileID, name string) string {
	base := senderFileName(name)
	if base == "" {
		sum := sha256.Sum256([]byte(fileID))
		base = "telegram-" + hex.EncodeToString(sum[:4]) + ".bin"
	}
	dst := filepath.Join(dir, base)
	if _, err := os.Stat(dst); err == nil {
		ext := filepath.Ext(base)
		stem := strings.TrimSuffix(base, ext)
		dst = filepath.Join(dir, fmt.Sprintf("%s.%d%s", stem, time.Now().UnixNano(), ext))
	}
	return dst
}
