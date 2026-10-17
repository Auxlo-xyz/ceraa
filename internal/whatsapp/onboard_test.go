// Pure onboarding tests (onboard.go). No network: phone validation, the
// login status machine, the QR stub contract, and the pair-code inlet are
// all exercised with scripted inputs only.
package whatsapp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNormalizePhoneStripsFormatting(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"spaces", "+234 801 234 5678", "2348012345678"},
		{"dashes and parens", "+1 (555) 123-4567", "15551234567"},
		{"already digits", "2348012345678", "2348012345678"},
		{"dots stripped", "234.801.234.5678", "2348012345678"},
		{"seven digit minimum", "2345678", "2345678"},
		{"fifteen digit maximum", "123456789012345", "123456789012345"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizePhone(tc.raw)
			if err != nil {
				t.Fatalf("NormalizePhone(%q) = %v, want nil", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("NormalizePhone(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestNormalizePhoneRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"only formatting", "+- () "},
		{"letters leave nothing", "abc"},
		{"leading zero", "08012345678"},
		{"leading zero with plus", "+08012345678"},
		{"too short", "123"},
		{"six digits", "123456"},
		{"too long", "1234567890123456"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := NormalizePhone(tc.raw); err == nil {
				t.Errorf("NormalizePhone(%q) = %q, want an error", tc.raw, got)
			}
		})
	}
}

// The full transition table: every state crossed with every event, read
// directly off NextLoginStatus. Unknown pairs stay put.
func TestNextLoginStatusTransitionTable(t *testing.T) {
	cases := []struct {
		name  string
		from  LoginStatus
		event LoginEvent
		want  LoginStatus
	}{
		{"NotLinked starts QR", NotLinked, EventStartQR, AwaitingScan},
		{"NotLinked starts code", NotLinked, EventStartCode, AwaitingCode},
		{"NotLinked ignores linked", NotLinked, EventLinked, NotLinked},
		{"NotLinked ignores expired", NotLinked, EventExpired, NotLinked},
		{"NotLinked reset stays", NotLinked, EventReset, NotLinked},

		{"scan links on scan", AwaitingScan, EventLinked, Linked},
		{"scan expires", AwaitingScan, EventExpired, Expired},
		{"scan re-issue stays", AwaitingScan, EventStartQR, AwaitingScan},
		{"scan switches to code", AwaitingScan, EventStartCode, AwaitingCode},
		{"scan resets", AwaitingScan, EventReset, NotLinked},

		{"code links on entry", AwaitingCode, EventLinked, Linked},
		{"code expires", AwaitingCode, EventExpired, Expired},
		{"code re-issue stays", AwaitingCode, EventStartCode, AwaitingCode},
		{"code switches to QR", AwaitingCode, EventStartQR, AwaitingScan},
		{"code resets", AwaitingCode, EventReset, NotLinked},

		{"linked survives QR start", Linked, EventStartQR, Linked},
		{"linked survives code start", Linked, EventStartCode, Linked},
		{"linked survives linked", Linked, EventLinked, Linked},
		{"linked survives expiry", Linked, EventExpired, Linked},
		{"linked resets", Linked, EventReset, NotLinked},

		{"expired re-issues QR", Expired, EventStartQR, AwaitingScan},
		{"expired re-issues code", Expired, EventStartCode, AwaitingCode},
		{"expired ignores linked", Expired, EventLinked, Expired},
		{"expired expiry stays", Expired, EventExpired, Expired},
		{"expired resets", Expired, EventReset, NotLinked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NextLoginStatus(tc.from, tc.event); got != tc.want {
				t.Errorf("NextLoginStatus(%v, %v) = %v, want %v",
					tc.from, tc.event, got, tc.want)
			}
		})
	}
}

func TestNextLoginStatusUnknownEventStays(t *testing.T) {
	states := []LoginStatus{NotLinked, AwaitingScan, AwaitingCode, Linked, Expired}
	for _, s := range states {
		if got := NextLoginStatus(s, LoginEvent(999)); got != s {
			t.Errorf("NextLoginStatus(%v, unknown) = %v, want the state to stay", s, got)
		}
	}
}

func TestLoginStatusStringsAreReadable(t *testing.T) {
	cases := map[LoginStatus]string{
		NotLinked:    "NotLinked",
		AwaitingScan: "AwaitingScan",
		AwaitingCode: "AwaitingCode",
		Linked:       "Linked",
		Expired:      "Expired",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("LoginStatus(%d).String() = %q, want %q", int(s), got, want)
		}
	}
	if got := LoginStatus(999).String(); got == "" {
		t.Error("out-of-range LoginStatus.String() is empty, want a fallback label")
	}
}

func TestLoginEventStringsAreReadable(t *testing.T) {
	events := []LoginEvent{EventStartQR, EventStartCode, EventLinked, EventExpired, EventReset}
	seen := map[string]bool{}
	for _, e := range events {
		s := e.String()
		if s == "" || s == "Unknown" {
			t.Errorf("LoginEvent(%d).String() = %q, want a readable label", int(e), s)
		}
		if seen[s] {
			t.Errorf("duplicate LoginEvent label %q", s)
		}
		seen[s] = true
	}
}

// RenderQRPNG encodes offline with the vendored pure-Go encoder. The PNG
// magic bytes pin the format the owner chat will receive as a photo.
func TestRenderQRPNGEncodesPNG(t *testing.T) {
	out, err := RenderQRPNG("test-code")
	if err != nil {
		t.Fatalf("RenderQRPNG = %v", err)
	}
	pngMagic := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	if len(out) < len(pngMagic) {
		t.Fatalf("RenderQRPNG returned %d bytes, want a full PNG", len(out))
	}
	for i, b := range pngMagic {
		if out[i] != b {
			t.Fatalf("RenderQRPNG byte %d = %#x, want PNG magic", i, out[i])
		}
	}
	if out, err := RenderQR("test-code"); err != nil || len(out) == 0 {
		t.Errorf("RenderQR = (%d bytes, %v), want PNG bytes and nil error", len(out), err)
	}
}

func TestRenderQRPNGRejectsEmptyCode(t *testing.T) {
	for _, code := range []string{"", "   "} {
		if out, err := RenderQRPNG(code); err == nil {
			t.Errorf("RenderQRPNG(%q) = %d bytes, want an error", code, len(out))
		}
	}
}

// stubPairClient scripts the PairPhone surface without any network.
type stubPairClient struct {
	code     string
	err      error
	gotPhone string
	calls    int
}

func (s *stubPairClient) PairPhone(ctx context.Context, phone string, _ bool, _, _ string) (string, error) {
	s.calls++
	s.gotPhone = phone
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.err != nil {
		return "", s.err
	}
	return s.code, nil
}

func TestRequestPairCodePassesNormalizedDigits(t *testing.T) {
	stub := &stubPairClient{code: "ABCD-1234"}
	code, err := RequestPairCode(context.Background(), stub, "+234 801 234 5678")
	if err != nil {
		t.Fatalf("RequestPairCode = %v, want nil", err)
	}
	if code != "ABCD-1234" {
		t.Errorf("code = %q, want the client code verbatim", code)
	}
	if stub.calls != 1 {
		t.Errorf("client called %d times, want 1", stub.calls)
	}
	if stub.gotPhone != "2348012345678" {
		t.Errorf("client got phone %q, want normalized digits", stub.gotPhone)
	}
}

func TestRequestPairCodeValidatesBeforeCalling(t *testing.T) {
	stub := &stubPairClient{code: "ABCD-1234"}
	if _, err := RequestPairCode(context.Background(), stub, "08012345678"); err == nil {
		t.Error("leading-zero phone accepted, want a validation error")
	}
	if _, err := RequestPairCode(context.Background(), stub, "12"); err == nil {
		t.Error("short phone accepted, want a validation error")
	}
	if stub.calls != 0 {
		t.Errorf("client called %d times for invalid phones, want 0", stub.calls)
	}
}

func TestRequestPairCodeRefusesBadSetup(t *testing.T) {
	stub := &stubPairClient{code: "ABCD-1234"}
	if _, err := RequestPairCode(context.Background(), nil, "+2348012345678"); err == nil {
		t.Error("nil client accepted, want an error")
	}
	if _, err := RequestPairCode(nil, stub, "+2348012345678"); err == nil {
		t.Error("nil context accepted, want an error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RequestPairCode(ctx, stub, "+2348012345678"); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled context gave %v, want context.Canceled", err)
	}
}

func TestRequestPairCodePropagatesClientErrors(t *testing.T) {
	stub := &stubPairClient{err: errors.New("pairing rejected")}
	if _, err := RequestPairCode(context.Background(), stub, "+2348012345678"); err == nil {
		t.Error("client error swallowed, want it propagated")
	} else if !strings.Contains(err.Error(), "pairing rejected") {
		t.Errorf("error = %v, want the client message inside", err)
	}
}

func TestRequestPairCodeLinkedRefusesBadSetup(t *testing.T) {
	if _, err := RequestPairCodeLinked(context.Background(), nil, "+2348012345678"); err == nil {
		t.Error("nil concrete client accepted, want an error")
	}
	if _, err := RequestPairCodeLinked(context.Background(), nil, "12"); err == nil {
		t.Error("bad phone with nil client accepted, want an error")
	}
}

func TestQRTimeoutWindows(t *testing.T) {
	if got := QRTimeout(true); got != 60*time.Second {
		t.Errorf("QRTimeout(first) = %v, want 60s", got)
	}
	if got := QRTimeout(false); got != 20*time.Second {
		t.Errorf("QRTimeout(repeat) = %v, want 20s", got)
	}
	if FirstQRTimeout != 60*time.Second || RepeatQRTimeout != 20*time.Second {
		t.Error("QR timeout constants drifted from the documented 60s/20s windows")
	}
}
