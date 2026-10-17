// Package whatsapp links the owner account on first run.
//
// Telegram wiring: the bridge drives onboarding and sends every message
// itself. For QR it calls GetQRChannel before Connect, renders each code
// with RenderQRPNG, and sends the PNG bytes as a photo. For the 8-char
// code flow it calls RequestPairCode and sends the returned string as
// text. On expiry it applies NextLoginStatus with EventExpired and then
// re-issues. Nothing here sends messages or starts a daemon.
package whatsapp

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
)

// RenderQRPNG encodes one QR channel code string as a 256px PNG image.
// Pure-Go encoder, no CGO, safe to cross-compile into the release binary.
func RenderQRPNG(code string) ([]byte, error) {
	if strings.TrimSpace(code) == "" {
		return nil, errors.New("whatsapp: empty QR code")
	}
	q, err := qrcode.New(code, qrcode.Medium)
	if err != nil {
		return nil, err
	}
	return q.PNG(256)
}

// RenderQR is the same encoder under the short onboarding name.
func RenderQR(code string) ([]byte, error) {
	return RenderQRPNG(code)
}

// PairClient is the narrow client surface onboarding needs. It mirrors
// the underlying client PairPhone shape, so the concrete client from
// PairingClient can be passed directly once vendored.
type PairClient interface {
	PairPhone(ctx context.Context, phone string, showPushNotification bool, clientType, clientDisplayName string) (string, error)
}

// Pairing display identity. Neutral Ceraa naming, no third-party brand.
const (
	pairClientType  = "Ceraa"
	pairDisplayName = "Ceraa (Linux)"
	maxPhoneDigits  = 15
	minPhoneDigits  = 7
)

// NormalizePhone strips non-digits and validates E.164-ish input.
func NormalizePhone(phone string) (string, error) {
	var b strings.Builder
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	d := b.String()
	if d == "" {
		return "", errors.New("whatsapp: phone number is empty")
	}
	if d[0] == '0' {
		return "", errors.New("whatsapp: phone number must use international format without leading 0")
	}
	if len(d) < minPhoneDigits {
		return "", errors.New("whatsapp: phone number too short")
	}
	if len(d) > maxPhoneDigits {
		return "", errors.New("whatsapp: phone number too long")
	}
	return d, nil
}

// RequestPairCode validates the phone and returns the 8-char link code
// verbatim, dash kept. The caller sends it to the owner.
func RequestPairCode(ctx context.Context, client PairClient, phone string) (string, error) {
	if ctx == nil {
		return "", errors.New("whatsapp: nil context")
	}
	if client == nil {
		return "", errors.New("whatsapp: nil client")
	}
	digits, err := NormalizePhone(phone)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	code, err := client.PairPhone(ctx, digits, true, pairClientType, pairDisplayName)
	if err != nil {
		return "", err
	}
	return code, nil
}

// RequestPairCodeLinked runs the code flow against the concrete linked
// client. The narrow PairClient above keeps offline tests hermetic; this
// adapter carries the real client type the narrow shape cannot name.
func RequestPairCodeLinked(ctx context.Context, cli *whatsmeow.Client, phone string) (string, error) {
	if cli == nil {
		return "", errors.New("whatsapp: nil client")
	}
	digits, err := NormalizePhone(phone)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return cli.PairPhone(ctx, digits, true, whatsmeow.PairClientChrome, pairDisplayName)
}

// LoginStatus is where owner linking stands.
type LoginStatus int

const (
	// NotLinked is the start state before any flow begins.
	NotLinked LoginStatus = iota
	// AwaitingScan waits for a QR code scan.
	AwaitingScan
	// AwaitingCode waits for the 8-char code entry on the phone.
	AwaitingCode
	// Linked means the owner account is connected.
	Linked
	// Expired means the last code lapsed and a new one is needed.
	Expired
)

// String keeps status readable in logs and owner messages.
func (s LoginStatus) String() string {
	switch s {
	case NotLinked:
		return "NotLinked"
	case AwaitingScan:
		return "AwaitingScan"
	case AwaitingCode:
		return "AwaitingCode"
	case Linked:
		return "Linked"
	case Expired:
		return "Expired"
	default:
		return "Unknown"
	}
}

// LoginEvent drives the status machine.
type LoginEvent int

const (
	// EventStartQR begins or re-issues the QR flow.
	EventStartQR LoginEvent = iota + 1
	// EventStartCode begins or re-issues the code flow.
	EventStartCode
	// EventLinked records a completed scan or code entry.
	EventLinked
	// EventExpired records a lapsed code.
	EventExpired
	// EventReset returns the machine to NotLinked.
	EventReset
)

// String keeps events readable in logs.
func (e LoginEvent) String() string {
	switch e {
	case EventStartQR:
		return "StartQR"
	case EventStartCode:
		return "StartCode"
	case EventLinked:
		return "Linked"
	case EventExpired:
		return "Expired"
	case EventReset:
		return "Reset"
	default:
		return "Unknown"
	}
}

// NextLoginStatus is the pure transition function. Unknown pairs stay.
func NextLoginStatus(current LoginStatus, event LoginEvent) LoginStatus {
	switch current {
	case NotLinked:
		switch event {
		case EventStartQR:
			return AwaitingScan
		case EventStartCode:
			return AwaitingCode
		}
	case AwaitingScan:
		switch event {
		case EventLinked:
			return Linked
		case EventExpired:
			return Expired
		case EventStartQR:
			return AwaitingScan
		case EventStartCode:
			return AwaitingCode
		case EventReset:
			return NotLinked
		}
	case AwaitingCode:
		switch event {
		case EventLinked:
			return Linked
		case EventExpired:
			return Expired
		case EventStartCode:
			return AwaitingCode
		case EventStartQR:
			return AwaitingScan
		case EventReset:
			return NotLinked
		}
	case Expired:
		switch event {
		case EventStartQR:
			return AwaitingScan
		case EventStartCode:
			return AwaitingCode
		case EventReset:
			return NotLinked
		}
	case Linked:
		if event == EventReset {
			return NotLinked
		}
	}
	return current
}

// Pairing timeouts. The first QR code lasts about 60s, later ones about
// 20s. The bridge re-issues after each lapse via EventExpired.
const (
	FirstQRTimeout  = 60 * time.Second
	RepeatQRTimeout = 20 * time.Second
	PairCodeTimeout = 60 * time.Second
)

// QRTimeout returns the expiry window for a QR code issue.
func QRTimeout(first bool) time.Duration {
	if first {
		return FirstQRTimeout
	}
	return RepeatQRTimeout
}
