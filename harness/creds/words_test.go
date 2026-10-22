package creds

import (
	"fmt"
	"sort"
	"testing"
	"time"
)

// The username derivation is the difference between a spray of fourteen role
// accounts and a spray of two hundred guesses. It has to be derived, and it
// has to say where each candidate came from.

// TestTheUsernamesAreDerivedFromTheDomain: the domain, its local part, the
// dot variant, the role names, the role names at the domain and the seeds.
func TestTheUsernamesAreDerivedFromTheDomain(t *testing.T) {
	names := Usernames("example.com", []string{"jsmith"})
	for _, want := range []string{
		"example.com",
		"example",
		"e.example",
		"admin",
		"info",
		"support",
		"dev",
		"root",
		"test",
		"user",
		"security",
		"noc",
		"helpdesk",
		"postmaster",
		"webmaster",
		"sales",
		"billing",
		"admin@example.com",
		"info@example.com",
		"jsmith",
		"jsmith@example.com",
	} {
		if !contains(names, want) {
			t.Errorf("the derived usernames do not carry %q", want)
		}
	}
}

// TestTheUsernamesAreSortedAndDeduplicated: a list that repeats an account
// spends an attempt on it twice.
func TestTheUsernamesAreSortedAndDeduplicated(t *testing.T) {
	names := Usernames("example.com", []string{"admin", "example.com", "jsmith"})
	seen := make(map[string]bool)
	for _, name := range names {
		if seen[name] {
			t.Errorf("the username %q is listed twice", name)
		}
		seen[name] = true
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("the usernames are not sorted: %v", names)
	}
}

// TestTheUsernamesAreBounded: a derived list is a spray, and a spray is
// bounded.
func TestTheUsernamesAreBounded(t *testing.T) {
	seeds := make([]string, 0, 500)
	for i := 0; i < 500; i++ {
		seeds = append(seeds, fmt.Sprintf("user%d", i))
	}
	names := Usernames("example.com", seeds)
	if len(names) > maxUsernames {
		t.Errorf("usernames = %d, want at most %d", len(names), maxUsernames)
	}
}

// TestTheUsernameSourcesAreNamed: a list with no provenance is a list the
// model cannot reason about.
func TestTheUsernameSourcesAreNamed(t *testing.T) {
	candidates := UsernameCandidates("example.com", []string{"jsmith"})
	sources := make(map[string]string, len(candidates))
	for _, candidate := range candidates {
		sources[candidate.Username] = candidate.Source
	}
	for username, want := range map[string]string{
		"example.com":        "domain",
		"example":            "local part",
		"e.example":          "dot variant",
		"admin":              "role",
		"admin@example.com":  "role at domain",
		"jsmith":             "seed",
		"jsmith@example.com": "seed at domain",
	} {
		got, ok := sources[username]
		if !ok {
			t.Errorf("the candidate %q is missing", username)
			continue
		}
		if got != want {
			t.Errorf("the source of %q is %q, want %q", username, got, want)
		}
	}
}

// TestTheDomainIsReadFromAURL: the caller says a domain and says it as a
// URL half the time.
func TestTheDomainIsReadFromAURL(t *testing.T) {
	for _, raw := range []string{
		"https://example.com/login",
		"example.com",
		"EXAMPLE.COM",
		"example.com:8443",
		"example.com/",
	} {
		names := Usernames(raw, nil)
		if !contains(names, "example") {
			t.Errorf("the domain %q did not yield the local part: %v", raw, names)
		}
		if !contains(names, "admin@example.com") {
			t.Errorf("the domain %q did not yield the role at the domain: %v", raw, names)
		}
	}
}

// TestTheDefaultPasswordsAreTheListAProfessionalTestsFirst: the built-in
// list is what shows up in breach corpora and in the defaults people leave
// behind.
func TestTheDefaultPasswordsAreTheListAProfessionalTestsFirst(t *testing.T) {
	passwords := DefaultPasswords()
	if len(passwords) < 200 {
		t.Errorf("the built-in list holds %d passwords, want at least 200", len(passwords))
	}
	for _, want := range []string{
		"password",
		"123456",
		"qwerty",
		"letmein",
		"admin",
		"summer2026",
		"winter2025",
		"google",
		"liverpool",
		"january",
		"december2025",
	} {
		if !contains(passwords, want) {
			t.Errorf("the built-in list does not carry %q", want)
		}
	}
}

// TestTheDefaultPasswordsAreACopy: a caller that sorts the list must not
// change what the next caller gets.
func TestTheDefaultPasswordsAreACopy(t *testing.T) {
	first := DefaultPasswords()
	if len(first) == 0 {
		t.Fatal("the built-in list is empty")
	}
	first[0] = "mutated"
	second := DefaultPasswords()
	if second[0] == "mutated" {
		t.Error("the built-in list is shared between callers")
	}
}

// TestThePasswordsAreDerivedFromTheSeeds: a name the operator gave is a
// better guess than a generic list.
func TestThePasswordsAreDerivedFromTheSeeds(t *testing.T) {
	year := fmt.Sprintf("%d", time.Now().Year())
	passwords := Passwords([]string{"Acme Corp"})
	for _, want := range []string{
		"acmecorp",
		"Acmecorp",
		"acmecorp" + year,
		"Acmecorp" + year,
		"acmecorp!",
		"acmecorp123",
	} {
		if !contains(passwords, want) {
			t.Errorf("the derived list does not carry %q", want)
		}
	}
	if !contains(passwords, "password") {
		t.Error("the built-in list is missing from the derived list")
	}
}

// TestTheSeedIsReducedToItsName: a seed is a company, a product or a
// domain, and the useful part of a domain is its first label.
func TestTheSeedIsReducedToItsName(t *testing.T) {
	passwords := Passwords([]string{"acme.example.com"})
	if !contains(passwords, "acme") {
		t.Errorf("the seed was not reduced to its name: %v", firstN(passwords, 12))
	}
}

// TestThePasswordListIsBounded: a list is an input, and an input with no
// bound is a way to spend an operator's budget.
func TestThePasswordListIsBounded(t *testing.T) {
	seeds := make([]string, 0, 500)
	for i := 0; i < 500; i++ {
		seeds = append(seeds, fmt.Sprintf("company%d", i))
	}
	passwords := Passwords(seeds)
	if len(passwords) > maxPasswords {
		t.Errorf("passwords = %d, want at most %d", len(passwords), maxPasswords)
	}
}

// contains reports whether the list carries the value.
func contains(list []string, value string) bool {
	for _, entry := range list {
		if entry == value {
			return true
		}
	}
	return false
}

// firstN returns the head of a list, for an error message.
func firstN(list []string, n int) []string {
	if len(list) <= n {
		return list
	}
	return list[:n]
}
