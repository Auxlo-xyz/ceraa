package creds

// wordlist.go reads the caller's own lists: a username file and a
// user:password combo file.
//
// A file is an input, and an input with no bound is a way to spend an
// operator's rate budget on a file somebody dropped in the session workdir,
// so both loaders are bounded. Every error names the path, because a retry
// has to be able to correct it and a wordlist the model just wrote lives
// under a directory it may have spelled differently.

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// maxWordlistEntries bounds a loaded list.
const maxWordlistEntries = 100000

// maxComboLineBytes bounds one line of a combo file. A credential line is
// short; a line this long is a file somebody dropped in by mistake.
const maxComboLineBytes = 64 * 1024

// LoadCombos reads a "user:pass" list from a file.
//
// The line is split at the first colon, so a password may itself contain
// one. Blank lines and lines starting with # are skipped, the entries are
// trimmed, duplicates are dropped and the result is bounded at a hundred
// thousand.
//
// A line with no colon is an error rather than a silent skip: a combo file
// that is not a combo file is a mistake the caller has to know about,
// because a skip would leave the spray running against half the list.
func LoadCombos(path string) ([]Credential, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, fmt.Errorf("a combo file path is required")
	}
	file, err := os.Open(trimmed)
	if err != nil {
		return nil, fmt.Errorf("read the combo file %s: %w", trimmed, err)
	}
	defer file.Close()

	out := make([]Credential, 0, 64)
	seen := make(map[string]bool)
	line := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), maxComboLineBytes)
	for scanner.Scan() {
		line++
		entry := strings.TrimSpace(scanner.Text())
		if entry == "" || strings.HasPrefix(entry, "#") {
			continue
		}
		separator := strings.Index(entry, ":")
		if separator < 0 {
			return nil, fmt.Errorf("the combo file %s has no ':' separator on line %d", trimmed, line)
		}
		username := strings.TrimSpace(entry[:separator])
		password := strings.TrimSpace(entry[separator+1:])
		if username == "" {
			return nil, fmt.Errorf("the combo file %s has an empty username on line %d", trimmed, line)
		}
		key := username + "\x00" + password
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Credential{Username: username, Password: password, Source: "combo"})
		if len(out) >= maxWordlistEntries {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read the combo file %s: %w", trimmed, err)
	}
	return out, nil
}

// LoadUsers reads a username list from a file, one per line.
//
// Blank lines and lines starting with # are skipped, the entries are
// trimmed, duplicates are dropped and the result is bounded at a hundred
// thousand.
func LoadUsers(path string) ([]string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, fmt.Errorf("a username file path is required")
	}
	file, err := os.Open(trimmed)
	if err != nil {
		return nil, fmt.Errorf("read the username file %s: %w", trimmed, err)
	}
	defer file.Close()

	out := make([]string, 0, 64)
	seen := make(map[string]bool)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), maxComboLineBytes)
	for scanner.Scan() {
		entry := strings.TrimSpace(scanner.Text())
		if entry == "" || strings.HasPrefix(entry, "#") {
			continue
		}
		if seen[entry] {
			continue
		}
		seen[entry] = true
		out = append(out, entry)
		if len(out) >= maxWordlistEntries {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read the username file %s: %w", trimmed, err)
	}
	return out, nil
}
