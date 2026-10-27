package telegram

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMTProtoCredentialsEnvOverridesBaked(t *testing.T) {
	oldID, oldHash := bakedAPIID, bakedAPIHash
	defer func() { bakedAPIID, bakedAPIHash = oldID, oldHash }()

	bakedAPIID, bakedAPIHash = "111", "bakedhash"
	t.Setenv("CERAA_TG_API_ID", "222")
	t.Setenv("CERAA_TG_API_HASH", "envhash")

	id, hash, ok := mtprotoCredentials()
	if !ok || id != 222 || hash != "envhash" {
		t.Fatalf("env override should win: id=%d hash=%q ok=%v", id, hash, ok)
	}
}

func TestMTProtoCredentialsFallBackToBaked(t *testing.T) {
	oldID, oldHash := bakedAPIID, bakedAPIHash
	defer func() { bakedAPIID, bakedAPIHash = oldID, oldHash }()

	bakedAPIID, bakedAPIHash = "333", "bakedhash"
	os.Unsetenv("CERAA_TG_API_ID")
	os.Unsetenv("CERAA_TG_API_HASH")

	id, hash, ok := mtprotoCredentials()
	if !ok || id != 333 || hash != "bakedhash" {
		t.Fatalf("baked values should serve: id=%d hash=%q ok=%v", id, hash, ok)
	}
}

func TestMTProtoCredentialsMissing(t *testing.T) {
	oldID, oldHash := bakedAPIID, bakedAPIHash
	defer func() { bakedAPIID, bakedAPIHash = oldID, oldHash }()

	bakedAPIID, bakedAPIHash = "", ""
	os.Unsetenv("CERAA_TG_API_ID")
	os.Unsetenv("CERAA_TG_API_HASH")

	if _, _, ok := mtprotoCredentials(); ok {
		t.Fatal("empty credential must read as unavailable")
	}

	bakedAPIID, bakedAPIHash = "not-a-number", "x"
	if _, _, ok := mtprotoCredentials(); ok {
		t.Fatal("a non-numeric app id must read as unavailable")
	}
}

func TestMTProtoDestPathUsesSenderName(t *testing.T) {
	dir := t.TempDir()
	got := mtprotoDestPath(dir, "AAAAbbbbCCCCdddd", "report.final.pdf")
	if filepath.Base(got) != "report.final.pdf" {
		t.Fatalf("sender name should be kept: %s", got)
	}
}

func TestMTProtoDestPathHashHandleAndCollision(t *testing.T) {
	dir := t.TempDir()
	first := mtprotoDestPath(dir, "fileid-one", "")
	if filepath.Base(first) == "" || filepath.Base(first) == ".bin" {
		t.Fatalf("hash handle should be a real name: %s", first)
	}
	if err := os.WriteFile(first, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := mtprotoDestPath(dir, "fileid-one", "")
	if second == first {
		t.Fatalf("a taken handle must not collide: %s", first)
	}
	if filepath.Dir(second) != dir {
		t.Fatalf("collision should stay in the same directory: %s", second)
	}
}

func TestMTProtoDestPathRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	got := mtprotoDestPath(dir, "fileid", "../../etc/passwd")
	if filepath.Dir(got) != dir {
		t.Fatalf("traversal must not escape the files directory: %s", got)
	}
}
