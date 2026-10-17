package whatsapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The hub edits this file, so the round trip is the contract: a number typed in
// one format is stored once, in a format that matches the sender on any server.
func TestAddAllowListEntryNormalizesAndDedupes(t *testing.T) {
	restore := SetAllowListPathForTest(filepath.Join(t.TempDir(), "allowlist.json"))
	defer restore()

	entry, added, err := AddAllowListEntry("234 801-234-5678")
	if err != nil {
		t.Fatalf("add formatted number: %v", err)
	}
	if entry != "2348012345678" || !added {
		t.Fatalf("add = (%q, %v), want (2348012345678, true)", entry, added)
	}
	if entry, added, err = AddAllowListEntry("+2348012345678"); err != nil || added || entry != "2348012345678" {
		t.Fatalf("duplicate add = (%q, %v, %v), want dedupe with added=false", entry, added, err)
	}
	stored := LoadAllowList()
	if len(stored) != 1 || stored[0] != "2348012345678" {
		t.Fatalf("stored = %v, want one entry", stored)
	}
}

// A value that is not a phone number must be refused rather than stored: an
// entry that never matches is worse than no entry, because it looks like it
// works.
func TestAddAllowListEntryRejectsNonNumbers(t *testing.T) {
	restore := SetAllowListPathForTest(filepath.Join(t.TempDir(), "allowlist.json"))
	defer restore()

	for _, raw := range []string{"", "   ", "abc", "12345", "0123456789", "2348012345678901", "12036304@g.us", "2348012345678@s.whatsapp.net"} {
		if _, added, err := AddAllowListEntry(raw); err == nil || added {
			t.Errorf("AddAllowListEntry(%q) = added=%v err=%v, want refused", raw, added, err)
		}
	}
	if stored := LoadAllowList(); len(stored) != 0 {
		t.Fatalf("a refused entry was stored: %v", stored)
	}
}

func TestRemoveAllowListEntry(t *testing.T) {
	restore := SetAllowListPathForTest(filepath.Join(t.TempDir(), "allowlist.json"))
	defer restore()

	for _, number := range []string{"11111111", "22222222"} {
		if _, _, err := AddAllowListEntry(number); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := RemoveAllowListEntry("11111111@s.whatsapp.net")
	if err != nil || !removed {
		t.Fatalf("remove = (%v, %v), want removed", removed, err)
	}
	if stored := LoadAllowList(); len(stored) != 1 || stored[0] != "22222222" {
		t.Fatalf("stored after remove = %v", stored)
	}
	if removed, err := RemoveAllowListEntry("11111111"); err != nil || removed {
		t.Fatalf("second remove = (%v, %v), want not removed", removed, err)
	}
}

// The effective list is both halves, deduped, in order. The environment stays
// the operator's seed; the store is what the hub edits.
func TestEffectiveAllowListMergesEnvAndStore(t *testing.T) {
	restore := SetAllowListPathForTest(filepath.Join(t.TempDir(), "allowlist.json"))
	defer restore()
	t.Setenv("CERAA_WHATSAPP_ALLOW_JIDS", "1111111@s.whatsapp.net, 2222222")

	if _, _, err := AddAllowListEntry("2222222"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := AddAllowListEntry("3333333"); err != nil {
		t.Fatal(err)
	}
	got := EffectiveAllowList()
	want := []string{"1111111@s.whatsapp.net", "2222222", "3333333"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("EffectiveAllowList = %v, want %v", got, want)
	}
	if env := EnvAllowList(); len(env) != 2 {
		t.Fatalf("EnvAllowList = %v, want the two environment entries", env)
	}
}

// A missing or corrupt store must read as empty, not as an error that locks the
// owner out: the environment seed and the owner-only fallback still apply.
func TestLoadAllowListToleratesMissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	restore := SetAllowListPathForTest(filepath.Join(dir, "allowlist.json"))
	defer restore()

	if got := LoadAllowList(); got != nil {
		t.Fatalf("missing file reads as %v, want nil", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "allowlist.json"), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LoadAllowList(); got != nil {
		t.Fatalf("corrupt file reads as %v, want nil", got)
	}
}

// Allowed is the whole point: a number the hub stored is admitted on the next
// message, with no restart.
func TestAllowedHonorsStoredEntry(t *testing.T) {
	restore := SetAllowListPathForTest(filepath.Join(t.TempDir(), "allowlist.json"))
	defer restore()

	self := "9999999999@s.whatsapp.net"
	if Allowed("2348012345678@s.whatsapp.net", self) {
		t.Fatal("a number is allowed before it was stored")
	}
	if !Allowed(self, self) {
		t.Fatal("owner-only fallback: the linked account itself must pass")
	}
	if _, _, err := AddAllowListEntry("2348012345678"); err != nil {
		t.Fatal(err)
	}
	if !Allowed("2348012345678@s.whatsapp.net", self) {
		t.Fatal("a stored number is not allowed")
	}
	if !Allowed("2348012345678@lid", self) {
		t.Fatal("a stored number must match across servers")
	}
}

func TestSameNumberComparesDigits(t *testing.T) {
	if !SameNumber("2348012345678@s.whatsapp.net", "2348012345678") {
		t.Error("same digits on different servers should match")
	}
	if SameNumber("2348012345678", "2348012345679") {
		t.Error("different numbers should not match")
	}
	if SameNumber("", "2348012345678") {
		t.Error("an empty value should never match")
	}
}
