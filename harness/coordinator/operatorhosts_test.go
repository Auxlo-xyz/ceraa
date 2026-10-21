package coordinator

// operatorhosts_test.go pins the signal the recon layer reads: the hosts the
// operator typed, derived from the transcript rather than from a form.
//
// The failure this guards against is a harness that authorizes a request
// because the model asked nicely, and refuses one because nobody filled in
// a field. The authorization has to come from the human's own words.

import (
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
)

func TestOperatorHostsReadsTheOperatorTurns(t *testing.T) {
	sess := &sessionstore.Session{Messages: []sessionstore.Message{
		{Role: llm.RoleSystem, Content: "system prompt"},
		{Role: llm.RoleUser, Content: "run recon on https://streamleapstudio.xyz/app"},
		{Role: llm.RoleAssistant, Content: "probing api.example.com"},
		{Role: llm.RoleUser, Content: "also check example.org"},
	}}
	hosts := operatorHosts(sess)
	if hosts == "" {
		t.Fatal("no hosts read from the transcript")
	}
	for _, want := range []string{"streamleapstudio.xyz", "example.org"} {
		if !containsHost(hosts, want) {
			t.Errorf("operator hosts %q does not carry %q", hosts, want)
		}
	}
	if containsHost(hosts, "api.example.com") {
		t.Errorf("operator hosts %q carries a host only the model said", hosts)
	}
}

func TestOperatorHostsIgnoresTheModelTurns(t *testing.T) {
	sess := &sessionstore.Session{Messages: []sessionstore.Message{
		{Role: llm.RoleAssistant, Content: "I will probe invented.example.net"},
	}}
	if hosts := operatorHosts(sess); hosts != "" {
		t.Errorf("the model's own words produced hosts: %q", hosts)
	}
}

func TestOperatorHostsOnAnEmptySession(t *testing.T) {
	if hosts := operatorHosts(nil); hosts != "" {
		t.Errorf("a nil session produced hosts: %q", hosts)
	}
}

func containsHost(list, host string) bool {
	for _, candidate := range splitHosts(list) {
		if candidate == host {
			return true
		}
	}
	return false
}

func splitHosts(list string) []string {
	var out []string
	current := ""
	for _, r := range list {
		if r == ',' {
			out = append(out, current)
			current = ""
			continue
		}
		current += string(r)
	}
	if current != "" {
		out = append(out, current)
	}
	return out
}
