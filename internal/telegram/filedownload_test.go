package telegram

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// stubTelegramBases points the API and file hosts at one test server for the
// duration of a test. The file host is a separate variable because the real
// one is a hardcoded host that no amount of API stubbing can reach.
func stubTelegramBases(t *testing.T, base string) {
	t.Helper()
	prevAPI, prevFile := telegramAPIBase, telegramFileBase
	telegramAPIBase = base + "/bot"
	telegramFileBase = base + "/file/bot"
	t.Cleanup(func() { telegramAPIBase, telegramFileBase = prevAPI, prevFile })
}

// Telegram caps bot downloads at 20 MB. A file past it has to be refused with
// that reason before a request is placed: getFile would only answer with a
// sentence the notice used to throw away, leaving the sender to guess whether
// the file was corrupt, expired or simply too big.
func TestDownloadRefusesAFilePastTheBotApiCapWithoutCallingTelegram(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"file_path":"documents/file_1"}}`))
	}))
	defer server.Close()
	stubTelegramBases(t, server.URL)

	_, err := downloadTelegramFile("token", "FILE_ID", telegramDownloadLimit+1)
	if err == nil {
		t.Fatal("an over-the-cap file was accepted")
	}
	if !strings.Contains(err.Error(), "20 MB") {
		t.Errorf("error = %q, want it to name the cap", err)
	}
	if !strings.Contains(err.Error(), "20.0 MB") {
		t.Errorf("error = %q, want it to say how big the file is", err)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("requests = %d, want 0: the refusal needs no round trip", got)
	}
}

// The description Telegram returns is the only place the real reason lives.
// Collapsing it to "getFile failed" is what turned a size rejection into a
// bare "(download failed)" in the chat.
func TestDownloadKeepsTheDescriptionTelegramGave(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: file is too big"}`))
	}))
	defer server.Close()
	stubTelegramBases(t, server.URL)

	_, err := downloadTelegramFile("token", "FILE_ID", 0)
	if err == nil {
		t.Fatal("a refused getFile was reported as success")
	}
	if !strings.Contains(err.Error(), "file is too big") {
		t.Errorf("error = %q, want Telegram's own words", err)
	}
}

// A non-200 on the file host used to be written to disk and reported saved:
// the status line was never read. The body Telegram returns in that case is a
// JSON error, so the sender would have ended up with a .wav that is not one.
func TestDownloadReportsARejectedFileHostInsteadOfSavingIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "getFile") {
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_path":"documents/file_1"}}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Bad Request: file is too big"}`))
	}))
	defer server.Close()
	stubTelegramBases(t, server.URL)
	files := t.TempDir()
	t.Setenv("CERAA_FILES_DIR", files)

	_, err := downloadTelegramFile("token", "FILE_ID", 0)
	if err == nil {
		t.Fatal("a rejected download was reported as success")
	}
	if !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("error = %q, want the status", err)
	}
	entries, readErr := os.ReadDir(files)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Errorf("files dir holds %d entries, want 0: a failed download saves nothing", len(entries))
	}
}

// A body cut short by a deadline looks complete enough to write. The size
// Telegram announced is the only thing that catches it before the file is
// reported saved.
func TestDownloadRefusesABodyThatStoppedEarly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "getFile") {
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_path":"documents/file_1","file_size":1000}}`))
			return
		}
		_, _ = w.Write(make([]byte, 400))
	}))
	defer server.Close()
	stubTelegramBases(t, server.URL)
	files := t.TempDir()
	t.Setenv("CERAA_FILES_DIR", files)

	_, err := downloadTelegramFile("token", "FILE_ID", 0)
	if err == nil {
		t.Fatal("a truncated body was reported as success")
	}
	if !strings.Contains(err.Error(), "400 B") || !strings.Contains(err.Error(), "1000") {
		t.Errorf("error = %q, want both the part received and the part promised", err)
	}
	entries, readErr := os.ReadDir(files)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Errorf("files dir holds %d entries, want 0: a truncated download saves nothing", len(entries))
	}
}

// The path that does work, so the guards above cannot quietly break it.
func TestDownloadSavesAWholeFileUnderItsTelegramName(t *testing.T) {
	body := strings.Repeat("RIFF", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "getFile") {
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_path":"documents/file_9","file_size":` + strconv.Itoa(len(body)) + `}}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	stubTelegramBases(t, server.URL)
	files := t.TempDir()
	t.Setenv("CERAA_FILES_DIR", files)

	path, err := downloadTelegramFile("token", "FILE_ID", 0)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("saved %d bytes, want %d", len(got), len(body))
	}
	if filepath.Base(path) != "file_9" {
		t.Errorf("name = %q, want Telegram's own", filepath.Base(path))
	}
}
