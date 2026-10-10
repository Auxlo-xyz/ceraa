package llm

import (
	"bytes"
	"strings"
	"testing"
)

func TestFingerprintIdenticalBodies(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"hello"}]}`)
	a := Fingerprint(body)
	b := Fingerprint(bytes.Clone(body))
	if a != b {
		t.Fatalf("identical bodies gave different fingerprints: %q vs %q", a, b)
	}
	if got := CompareFingerprints(a, b); got != "" {
		t.Fatalf("stable prefix should compare empty, got %q", got)
	}
}

func TestFingerprintEarlyByteChange(t *testing.T) {
	base := bytes.Repeat([]byte("a"), 2048)
	mut := bytes.Clone(base)
	mut[512] ^= 0x01
	a := Fingerprint(base)
	b := Fingerprint(mut)
	if a == b {
		t.Fatal("one-byte change in first 1KB did not change fingerprint")
	}
	desc := CompareFingerprints(a, b)
	if desc == "" {
		t.Fatal("expected non-empty change description")
	}
	if strings.Contains(desc, "\n") {
		t.Fatalf("description must be one line, got %q", desc)
	}
	if !strings.Contains(desc, "early bytes") {
		t.Fatalf("expected early-byte description, got %q", desc)
	}
}

func TestFingerprintAppendPastWindow(t *testing.T) {
	base := bytes.Repeat([]byte("b"), PrefixAuditWindow+1024)
	extended := append(bytes.Clone(base), []byte("tail-append")...)
	a := Fingerprint(base)
	b := Fingerprint(extended)
	if a == b {
		t.Fatal("append past 32KB did not change fingerprint")
	}
	aHash := a[:strings.LastIndex(a, ":")]
	bHash := b[:strings.LastIndex(b, ":")]
	if aHash != bHash {
		t.Fatalf("append past window should keep prefix hash stable: %q vs %q", a, b)
	}
	desc := CompareFingerprints(a, b)
	if desc == "" {
		t.Fatal("expected non-empty change description for length-only change")
	}
	if !strings.Contains(desc, "length changed") {
		t.Fatalf("expected length-change description, got %q", desc)
	}
}

func TestFingerprintEmptyBody(t *testing.T) {
	var nilBody []byte
	a := Fingerprint(nilBody)
	b := Fingerprint([]byte{})
	if a == "" || b == "" {
		t.Fatal("empty body must produce a non-empty fingerprint")
	}
	if a != b {
		t.Fatalf("nil and empty bodies should match: %q vs %q", a, b)
	}
	if got := CompareFingerprints(a, a); got != "" {
		t.Fatalf("empty fingerprint should compare stable, got %q", got)
	}
}

func TestFingerprintPrefixAndLengthChange(t *testing.T) {
	a := Fingerprint([]byte("hello world"))
	b := Fingerprint([]byte("HELLO world plus more"))
	desc := CompareFingerprints(a, b)
	if desc == "" {
		t.Fatal("expected non-empty description when hash and length both change")
	}
	if !strings.Contains(desc, "length changed") {
		t.Fatalf("expected length mention in combined change, got %q", desc)
	}
}
