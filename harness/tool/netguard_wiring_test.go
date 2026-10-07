package tool

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"ceraa/harness/netguard"
	"ceraa/harness/operation"
)

// The guard is tested inside its own package, which proves nothing about whether
// the tools call it. These are the tests that matter for the actual risk: a
// model asking for a URL and being refused.
//
// The browser is the one that matters most, because needsPermission deliberately
// exempts it, so it is the cheapest way for the agent to reach the host's own
// network.

func TestBrowserRefusesToNavigateOntoThisHostsNetwork(t *testing.T) {
	t.Setenv(netguard.AllowPrivateEnv, "")
	tool := NewBrowserTool()
	cases := map[string]string{
		"cloud metadata": "http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		"loopback":       "http://127.0.0.1:1/",
		"loopback alias": "http://localhost:1/",
		"private range":  "http://10.0.0.1/admin",
		"gcp metadata":   "http://metadata.google.internal/computeMetadata/v1/",
	}
	for name, target := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := tool.Translate(map[string]any{"action": "open", "url": target}); err == nil {
				t.Fatalf("the browser accepted %s", target)
			}
			// The second path resolves the target too, so it has to refuse as well.
			if _, err := browserOpenTarget(map[string]any{"url": target}); err == nil {
				t.Fatalf("browserOpenTarget accepted %s", target)
			}
		})
	}
}

func TestBrowserStillAllowsAPublicAddress(t *testing.T) {
	t.Setenv(netguard.AllowPrivateEnv, "")
	tool := NewBrowserTool()
	if _, err := tool.Translate(map[string]any{"action": "open", "url": "https://example.com/docs"}); err != nil {
		t.Fatalf("a public URL was refused: %v", err)
	}
}

// The refusal has to survive to the model, because the model is the only thing
// that can do something about it. An error that says nothing is a dead end.
func TestTheBrowserRefusalNamesTheAddress(t *testing.T) {
	t.Setenv(netguard.AllowPrivateEnv, "")
	tool := NewBrowserTool()
	_, err := tool.Translate(map[string]any{"action": "open", "url": "http://169.254.169.254/latest/"})
	if err == nil {
		t.Fatal("the metadata address was accepted")
	}
	if !errors.Is(err, netguard.ErrBlocked) {
		t.Errorf("the refusal is not detectable as a block: %v", err)
	}
	if !strings.Contains(err.Error(), "169.254.169.254") {
		t.Errorf("the refusal does not name the address: %v", err)
	}
}

// A local file is a deliberate capability and must survive the network guard.
// Rendering the page the agent just wrote is not an SSRF.
func TestBrowserOpenByLocalPathStillWorks(t *testing.T) {
	t.Setenv(netguard.AllowPrivateEnv, "")
	tool := NewBrowserTool()
	dir := t.TempDir()
	page := dir + "/report.html"
	if err := os.WriteFile(page, []byte("<html><body>ok</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := tool.Translate(map[string]any{"action": "open", "path": page})
	if err != nil {
		t.Fatalf("open by local path was refused: %v", err)
	}
	target, err := browserOpenTarget(spec.Args)
	if err != nil {
		t.Fatalf("the local target was refused: %v", err)
	}
	if !strings.HasPrefix(target, "file://") {
		t.Fatalf("target = %q, want a file URL", target)
	}
}

// Webfetch is the other egress, and the one that is easy to point anywhere
// because a URL is all it takes.
func TestWebFetchRefusesThisHostsNetwork(t *testing.T) {
	t.Setenv(netguard.AllowPrivateEnv, "")
	tool := &WebFetchTool{}
	for _, target := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:1/",
		"http://192.168.1.1/",
	} {
		spec, err := tool.Translate(map[string]any{"url": target})
		if err != nil {
			t.Fatalf("translate of %s failed before it should have reached the guard: %v", target, err)
		}
		if _, err := tool.RunContext(context.Background(), spec); err == nil {
			t.Fatalf("webfetch reached %s", target)
		} else if !errors.Is(err, netguard.ErrBlocked) {
			t.Errorf("%s refused with an unrelated error: %v", target, err)
		}
	}
}

// Asserted against the guard rather than by making a request. A unit test that
// dials out is a test that fails on a bad network day and says nothing about the
// guard, and one pointed at a public host is a test that reaches a stranger's
// server to prove a local function works.
func TestAPublicAddressIsNotBlocked(t *testing.T) {
	t.Setenv(netguard.AllowPrivateEnv, "")
	for _, target := range []string{"http://93.184.216.34/", "https://example.com/x", "http://8.8.8.8:53/"} {
		if err := netguard.CheckURL(context.Background(), target); err != nil {
			t.Errorf("%s was blocked: %v", target, err)
		}
	}
	spec, err := (&WebFetchTool{}).Translate(map[string]any{"url": "https://example.com/x"})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if spec.Type != "webfetch" || spec.Args["url"] != "https://example.com/x" {
		t.Fatalf("a public URL did not translate intact: %+v", spec)
	}
}

// The escape hatch has to work end to end, or "look at the app I am running"
// becomes impossible and the guard gets disabled instead of configured.
func TestTheOverrideReopensLoopbackForBothTools(t *testing.T) {
	t.Setenv(netguard.AllowPrivateEnv, "127.0.0.0/8")
	tool := NewBrowserTool()
	if _, err := tool.Translate(map[string]any{"action": "open", "url": "http://127.0.0.1:1/"}); err != nil {
		t.Fatalf("an explicitly permitted range was refused: %v", err)
	}
	// Port 1 rather than the live bot's 8787. The point is whether the guard
	// refuses, and a test that reaches a running service proves nothing extra
	// while coupling the suite to whatever happens to be listening.
	fetch := &WebFetchTool{}
	spec, err := fetch.Translate(map[string]any{"url": "http://127.0.0.1:1/", "timeout": 1})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if _, runErr := fetch.RunContext(context.Background(), spec); runErr != nil && errors.Is(runErr, netguard.ErrBlocked) {
		t.Fatalf("the override did not reach webfetch: %v", runErr)
	}
	// The metadata address stays blocked even with the hatch open, because the
	// hatch named a loopback range and nothing else.
	if _, err := tool.Translate(map[string]any{"action": "open", "url": "http://169.254.169.254/"}); err == nil {
		t.Fatal("the override leaked to the metadata service")
	}
}

// A tool registered and not executed is offered to the model and fails on every
// call. The guard is only real if the tool that carries it still runs.
func TestTheGuardedToolsStillTranslateAndExecute(t *testing.T) {
	t.Setenv(netguard.AllowPrivateEnv, "")
	registry := DefaultRegistryWithStores(nil, nil)
	for _, name := range []string{"webfetch", "browser_open"} {
		if _, ok := registry.Get(name); !ok {
			t.Errorf("%s is not registered", name)
		}
	}
	var spec operation.Spec
	spec, err := (&WebFetchTool{}).Translate(map[string]any{"url": "http://169.254.169.254/"})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if spec.Type != "webfetch" {
		t.Fatalf("spec.Type = %q", spec.Type)
	}
}
