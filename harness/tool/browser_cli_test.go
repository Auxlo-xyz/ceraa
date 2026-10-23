package tool

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

func TestBrowserCLIFlagsArePositionIndependent(t *testing.T) {
	for _, args := range [][]string{
		{"https://example.com", "--wait", "500"},
		{"--wait", "500", "https://example.com"},
		{"--wait=500", "https://example.com"},
	} {
		rest, flags, err := splitBrowserCLIFlags(args, nil, []string{"wait"})
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if len(rest) != 1 || rest[0] != "https://example.com" {
			t.Errorf("%v: positional = %v", args, rest)
		}
		wait, err := flags.intValue("wait")
		if err != nil || wait != 500 {
			t.Errorf("%v: wait = %d, err = %v", args, wait, err)
		}
	}
}

func TestBrowserCLIBoolFlagsAndRejections(t *testing.T) {
	rest, flags, err := splitBrowserCLIFlags([]string{"@1", "hello", "world", "--submit"}, []string{"submit"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !flags.has("submit") {
		t.Error("--submit was dropped")
	}
	if strings.Join(rest, " ") != "@1 hello world" {
		t.Errorf("positional = %v", rest)
	}
	if _, _, err := splitBrowserCLIFlags([]string{"--nope"}, nil, nil); err == nil {
		t.Error("an unknown flag was accepted")
	}
	if _, _, err := splitBrowserCLIFlags([]string{"--wait"}, nil, []string{"wait"}); err == nil {
		t.Error("a value flag with no value was accepted")
	}
}

func TestSnapshotRefsBecomeSelectors(t *testing.T) {
	if got := browserCLITarget("@7"); got != `[data-ceraa-ref="7"]` {
		t.Errorf("browserCLITarget(@7) = %q", got)
	}
	if got := browserCLITarget("#submit"); got != "#submit" {
		t.Errorf("a CSS selector was rewritten to %q", got)
	}
	if value, ok := parseBrowserRef("@12"); !ok || value != 12 {
		t.Errorf("parseBrowserRef(@12) = %d, %v", value, ok)
	}
	if _, ok := parseBrowserRef("@x"); ok {
		t.Error("parseBrowserRef accepted a non-numeric ref")
	}
}

func TestBrowserCLIUsageNamesEveryAction(t *testing.T) {
	usage := browserCLIUsage()
	for _, action := range []string{
		"open", "snapshot", "text", "html", "click", "type", "eval", "shot", "viewport", "status", "close",
	} {
		if !strings.Contains(usage, action) {
			t.Errorf("usage does not name the %q action", action)
		}
	}
}

func TestBrowserCLIRejectsAnUnknownActionWithoutLaunching(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	var out bytes.Buffer
	err := RunBrowserCLI(context.Background(), &out, []string{"teleport"})
	if err == nil || !strings.Contains(err.Error(), "unknown browser action") {
		t.Fatalf("err = %v", err)
	}
}

func TestBrowserCLIStatusNeverLaunchesABrowser(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	var out bytes.Buffer
	if err := RunBrowserCLI(context.Background(), &out, []string{"status"}); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "no browser is open" {
		t.Errorf("status = %q", out.String())
	}
	if _, ok := loadBrowserCLISession(); ok {
		t.Error("status recorded a session, so it launched a browser to say none was running")
	}
}

func TestBrowserCLIOpenRefusesWhatTheToolRefuses(t *testing.T) {
	for _, args := range [][]string{
		{"open", "file:///etc/passwd"},
		{"open", "javascript:alert(1)"},
		{"open"},
		{"open", "--file", "/tmp/x", "https://example.com"},
	} {
		if _, err := browserCLIOpenTarget(args[1:], openFileFlag(args)); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
	if target, err := browserCLIOpenTarget([]string{"https://example.com"}, ""); err != nil || target != "https://example.com" {
		t.Errorf("a public URL was refused: %q, %v", target, err)
	}
}

func openFileFlag(args []string) string {
	for index, arg := range args {
		if arg == "--file" && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}

func TestBrowserCLIViewportParsesTheSizesPeopleWrite(t *testing.T) {
	for _, raw := range []string{"390x844", "390X844", "390*844", " 390 x 844 ", "390×844"} {
		width, height, err := parseBrowserCLIViewport(raw)
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if width != 390 || height != 844 {
			t.Errorf("%q parsed as %dx%d", raw, width, height)
		}
	}
	for _, raw := range []string{"", "390", "390x", "x844", "0x844", "abc", "390x844x2", "99999999x1"} {
		if _, _, err := parseBrowserCLIViewport(raw); err == nil {
			t.Errorf("%q was accepted", raw)
		}
	}
}

func TestBrowserCLIViewportFlagsCompose(t *testing.T) {
	current := browserCLIViewport{Width: 1280, Height: 800}
	cases := []struct {
		name  string
		args  []string
		want  string
		error bool
	}{
		{name: "phone", args: []string{"--viewport", "390x844", "--mobile", "--dpr", "2"}, want: "390x844 mobile dpr 2"},
		{name: "width only keeps height", args: []string{"--width", "1024"}, want: "1024x800 desktop dpr 1"},
		{name: "desktop after mobile", args: []string{"--mobile", "--desktop"}, error: true},
		{name: "desktop resets the ratio", args: []string{"--desktop"}, want: "1280x800 desktop dpr 1"},
		{name: "desktop keeps an explicit ratio", args: []string{"--desktop", "--dpr", "2"}, want: "1280x800 desktop dpr 2"},
		{name: "dpr too high", args: []string{"--dpr", "9"}, error: true},
		{name: "dpr too low", args: []string{"--dpr", "0.1"}, error: true},
	}
	for _, test := range cases {
		rest, flags, err := splitBrowserCLIFlags(test.args, []string{"mobile", "desktop"},
			[]string{"viewport", "width", "height", "dpr"})
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if len(rest) != 0 {
			t.Errorf("%s: positional = %v", test.name, rest)
		}
		have, changed, err := browserCLIViewportFromFlags(current, flags)
		if test.error {
			if err == nil {
				t.Errorf("%s: accepted", test.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if !changed {
			t.Errorf("%s: reported no change", test.name)
		}
		if have.String() != test.want {
			t.Errorf("%s: got %q, want %q", test.name, have.String(), test.want)
		}
	}
}

func TestBrowserCLIViewportDefaultsToDesktop(t *testing.T) {
	if got := (browserCLIViewport{}).String(); got != "1280x800 desktop dpr 1" {
		t.Errorf("default viewport = %q", got)
	}
	if _, changed, err := browserCLIViewportFromFlags(browserCLIViewport{}, browserCLIFlags{}); err != nil || changed {
		t.Errorf("an empty flag set reported changed=%v, err=%v", changed, err)
	}
}

func TestBrowserCLISessionRemembersTheViewport(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	phone := browserCLIViewport{Width: 390, Height: 844, Mobile: true, DPR: 2}
	if err := saveBrowserCLISession(browserCLISession{PID: 1, Port: 2, Viewport: &phone}); err != nil {
		t.Fatal(err)
	}
	session, ok := loadBrowserCLISession()
	if !ok {
		t.Fatal("the session did not load back")
	}
	if got := sessionBrowserCLIViewport(session).String(); got != "390x844 mobile dpr 2" {
		t.Errorf("remembered viewport = %q", got)
	}
	// A session written before viewports existed must read as the default,
	// not as a zero size that would blank the page.
	raw := []byte(`{"pid":1,"port":2,"last_used":0}`)
	if err := os.WriteFile(browserCLISessionFile(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	session, ok = loadBrowserCLISession()
	if !ok {
		t.Fatal("a legacy session did not load")
	}
	if got := sessionBrowserCLIViewport(session).String(); got != "1280x800 desktop dpr 1" {
		t.Errorf("legacy session viewport = %q", got)
	}
}

func TestBrowserCLIUsageTeachesTheViewport(t *testing.T) {
	usage := browserCLIUsage()
	for _, want := range []string{"--viewport 390x844", "--mobile", "--dpr", "780x1688"} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage does not mention %q", want)
		}
	}
	if !browserCLIActions["viewport"] {
		t.Error("viewport is missing from the action list")
	}
}
