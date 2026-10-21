package operator

import "testing"

func TestHostsFindsNamedTargets(t *testing.T) {
	text := "run recon on https://streamleapstudio.xyz/app and check api.example.com, 127.0.0.1:9443 and localhost too"
	hosts := Hosts(text)
	want := map[string]bool{
		"streamleapstudio.xyz": true,
		"api.example.com":      true,
		"127.0.0.1":            true,
		"localhost":            true,
	}
	for _, host := range hosts {
		delete(want, host)
	}
	if len(want) != 0 {
		t.Errorf("hosts %v missed %v", hosts, want)
	}
}

func TestHostsIgnoresFileNamesAndDeduplicates(t *testing.T) {
	hosts := Hosts("edit src/index.html then edit src/other.html for example.com and example.com again")
	for _, host := range hosts {
		if host == "index.html" || host == "other.html" {
			t.Errorf("a file name was read as a host: %v", hosts)
		}
	}
	count := 0
	for _, host := range hosts {
		if host == "example.com" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("example.com appeared %d times in %v", count, hosts)
	}
}

func TestCoversMatchesSubdomainsOnlyDownwards(t *testing.T) {
	named := []string{"example.com"}
	if !Covers(named, "example.com") {
		t.Error("the named host does not cover itself")
	}
	if !Covers(named, "api.example.com") {
		t.Error("the named host does not cover its subdomain")
	}
	if Covers(named, "example.com.evil.net") {
		t.Error("a lookalike suffix was accepted")
	}
	if Covers([]string{"api.example.com"}, "example.com") {
		t.Error("a named subdomain covered its parent")
	}
}

func TestJoinAndSplitRoundTrip(t *testing.T) {
	hosts := Hosts("example.com and api.example.com")
	joined := Join(hosts)
	back := Split(joined)
	if len(back) != len(hosts) {
		t.Fatalf("round trip lost hosts: %v -> %q -> %v", hosts, joined, back)
	}
	if !Covers(back, "api.example.com") {
		t.Errorf("the round trip lost the match: %v", back)
	}
}
