package creds

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The loaders read the caller's own files. Every error names the path,
// because a retry has to be able to correct it and a list the model just
// wrote lives under a directory it may have spelled differently.

// writeFile puts a list on disk and returns its path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// TestTheCombosAreReadFromTheFile: the line is split at the first colon, so
// a password may contain one.
func TestTheCombosAreReadFromTheFile(t *testing.T) {
	path := writeFile(t, "combos.txt", "admin:hunter2\nroot:toor:secret\n")
	combos, err := LoadCombos(path)
	if err != nil {
		t.Fatalf("LoadCombos: %v", err)
	}
	if len(combos) != 2 {
		t.Fatalf("combos = %d, want 2", len(combos))
	}
	if combos[0].Username != "admin" || combos[0].Password != "hunter2" {
		t.Errorf("combo = %+v", combos[0])
	}
	if combos[0].Source != "combo" {
		t.Errorf("source = %q, want combo", combos[0].Source)
	}
	if combos[1].Username != "root" || combos[1].Password != "toor:secret" {
		t.Errorf("combo = %+v, want the password to keep its colon", combos[1])
	}
}

// TestTheCombosSkipBlanksAndComments: a list with a header in it is a list
// with a comment in it.
func TestTheCombosSkipBlanksAndComments(t *testing.T) {
	path := writeFile(t, "combos.txt", "# a header\n\n  admin:hunter2  \n\n#admin:commented\n")
	combos, err := LoadCombos(path)
	if err != nil {
		t.Fatalf("LoadCombos: %v", err)
	}
	if len(combos) != 1 {
		t.Fatalf("combos = %+v, want one", combos)
	}
	if combos[0].Username != "admin" || combos[0].Password != "hunter2" {
		t.Errorf("combo = %+v, want it trimmed", combos[0])
	}
}

// TestTheCombosAreDeduplicated: the same credential twice spends the budget
// twice and learns nothing.
func TestTheCombosAreDeduplicated(t *testing.T) {
	path := writeFile(t, "combos.txt", "admin:hunter2\nadmin:hunter2\nadmin:other\n")
	combos, err := LoadCombos(path)
	if err != nil {
		t.Fatalf("LoadCombos: %v", err)
	}
	if len(combos) != 2 {
		t.Errorf("combos = %+v, want two", combos)
	}
}

// TestAComboWithoutASeparatorIsAnError: a file that is not a combo file is
// a mistake the caller has to know about, because a silent skip would leave
// the spray running against half the list.
func TestAComboWithoutASeparatorIsAnError(t *testing.T) {
	path := writeFile(t, "combos.txt", "admin:hunter2\nadminwithnoseparator\n")
	_, err := LoadCombos(path)
	if err == nil {
		t.Fatal("LoadCombos accepted a line with no separator")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %q, want it to name the path", err.Error())
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error = %q, want it to name the line", err.Error())
	}
}

// TestAMissingComboFileNamesThePath: the error has to be actionable.
func TestAMissingComboFileNamesThePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.txt")
	_, err := LoadCombos(path)
	if err == nil {
		t.Fatal("LoadCombos accepted a file that is not there")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %q, want it to name the path", err.Error())
	}
}

// TestTheUsersAreReadFromTheFile: one name per line.
func TestTheUsersAreReadFromTheFile(t *testing.T) {
	path := writeFile(t, "users.txt", "# accounts\n\nalice\nbob\n  carol  \nalice\n")
	users, err := LoadUsers(path)
	if err != nil {
		t.Fatalf("LoadUsers: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("users = %v, want three", users)
	}
	if users[0] != "alice" || users[1] != "bob" || users[2] != "carol" {
		t.Errorf("users = %v, want them trimmed and in order", users)
	}
}

// TestAMissingUserFileNamesThePath: the error has to be actionable.
func TestAMissingUserFileNamesThePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.txt")
	_, err := LoadUsers(path)
	if err == nil {
		t.Fatal("LoadUsers accepted a file that is not there")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %q, want it to name the path", err.Error())
	}
}

// TestTheLoadedListsAreBounded: a file is an input, and an input with no
// bound is a way to spend an operator's budget on a file somebody dropped in
// the session workdir.
func TestTheLoadedListsAreBounded(t *testing.T) {
	var combos strings.Builder
	var users strings.Builder
	for i := 0; i < maxWordlistEntries+50; i++ {
		fmt.Fprintf(&combos, "user%d:pass%d\n", i, i)
		fmt.Fprintf(&users, "user%d\n", i)
	}
	comboPath := writeFile(t, "combos.txt", combos.String())
	userPath := writeFile(t, "users.txt", users.String())

	loaded, err := LoadCombos(comboPath)
	if err != nil {
		t.Fatalf("LoadCombos: %v", err)
	}
	if len(loaded) != maxWordlistEntries {
		t.Errorf("combos = %d, want the bound %d", len(loaded), maxWordlistEntries)
	}
	loadedUsers, err := LoadUsers(userPath)
	if err != nil {
		t.Fatalf("LoadUsers: %v", err)
	}
	if len(loadedUsers) != maxWordlistEntries {
		t.Errorf("users = %d, want the bound %d", len(loadedUsers), maxWordlistEntries)
	}
}

// TestAnEmptyPathIsRefused: a path that is not a path is a mistake the
// caller can fix.
func TestAnEmptyPathIsRefused(t *testing.T) {
	if _, err := LoadCombos(""); err == nil {
		t.Error("LoadCombos accepted an empty path")
	}
	if _, err := LoadUsers(""); err == nil {
		t.Error("LoadUsers accepted an empty path")
	}
}
