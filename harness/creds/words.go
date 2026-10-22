package creds

// words.go is the vocabulary a credential test starts from: the usernames a
// target is likely to have and the passwords those accounts are likely to
// have.
//
// The lists are deliberately small and deliberately boring. A professional
// tests the obvious first, because the obvious is what is actually set: the
// role accounts, the year, the company name, the keyboard walk. Everything
// past that is a guess, and a guess costs the target's rate budget and the
// engagement's time.
//
// The password list is never printed. A password list in a chat transcript is
// a liability: it is a shopping list for whoever reads the transcript.

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// The bounds.
const (
	// maxUsernames bounds the derived username list. Two hundred is more
	// than the role accounts of any organisation, and a longer list is a
	// longer spray.
	maxUsernames = 200
	// maxPasswords bounds the derived password list.
	maxPasswords = 2000
	// maxSeedLength bounds a seed, because a seed is a name and a name is
	// short.
	maxSeedLength = 64
)

// roleNames are the local parts every organisation has.
//
// The list is the set of accounts that exist before anybody is tested: the
// role accounts, the mail accounts, the accounts a firewall rule and a
// billing system and a support queue all assume.
var roleNames = []string{
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
}

// UsernameCandidate is one derived username and the source it came from.
//
// The source travels with the candidate because a list of usernames with no
// provenance is a list the model cannot reason about: it needs to know which
// ones were derived from the domain and which ones the operator supplied.
type UsernameCandidate struct {
	Username string
	Source   string
}

// Usernames derives candidate usernames for a target.
//
// The sources, merged and deduplicated:
//
//   - the domain as written ("example.com"), because a login form may take
//     an address as the name;
//   - the local part of the domain ("example"), because a form often takes
//     the bare label;
//   - the local part with a dot variant ("e.example"), the shape a
//     first-initial surname convention takes;
//   - the role names every organisation has (admin, info, support, ...);
//   - the role names at the domain ("admin@example.com"), because that is
//     how the role names are spelled when the form takes an address;
//   - the operator's seeds, each also spelled at the domain.
//
// The result is sorted and bounded at two hundred.
func Usernames(domain string, seeds []string) []string {
	candidates := UsernameCandidates(domain, seeds)
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.Username)
	}
	return out
}

// UsernameCandidates derives the candidate usernames and names the source of
// each one.
func UsernameCandidates(domain string, seeds []string) []UsernameCandidate {
	host := normalizeDomain(domain)
	local := ""
	if host != "" {
		local = strings.SplitN(host, ".", 2)[0]
	}

	found := make(map[string]bool)
	out := make([]UsernameCandidate, 0, 64)
	add := func(username, source string) {
		username = strings.TrimSpace(username)
		if username == "" || found[username] {
			return
		}
		found[username] = true
		out = append(out, UsernameCandidate{Username: username, Source: source})
	}

	if host != "" {
		add(host, "domain")
	}
	if local != "" {
		add(local, "local part")
		if len(local) > 1 {
			add(local[:1]+"."+local, "dot variant")
		}
	}
	for _, role := range roleNames {
		add(role, "role")
		if host != "" {
			add(role+"@"+host, "role at domain")
		}
	}
	for _, seed := range seeds {
		trimmed := strings.TrimSpace(seed)
		if trimmed == "" {
			continue
		}
		add(trimmed, "seed")
		if host != "" && !strings.Contains(trimmed, "@") {
			add(trimmed+"@"+host, "seed at domain")
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	if len(out) > maxUsernames {
		out = out[:maxUsernames]
	}
	return out
}

// normalizeDomain reduces what the caller gave to a bare host.
//
// The caller says a domain, but says it as a URL half the time, and a URL
// carries a scheme, a path and a port that are not part of the name.
func normalizeDomain(raw string) string {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return ""
	}
	if idx := strings.Index(trimmed, "://"); idx >= 0 {
		trimmed = trimmed[idx+3:]
	}
	if idx := strings.IndexAny(trimmed, "/?#"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	if idx := strings.Index(trimmed, ":"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	trimmed = strings.Trim(trimmed, ".")
	return trimmed
}

// Passwords returns a candidate password list: the built-in list plus the
// passwords derived from the seeds.
//
// A seed is usually a company name or a domain, and "Acme2026" is the first
// thing anybody tries against it. The built-in list is everything else, and
// it is tried after the derived ones: a name the operator gave is a better
// guess than a generic list.
func Passwords(seeds []string) []string {
	year := fmt.Sprintf("%d", time.Now().Year())
	found := make(map[string]bool)
	out := make([]string, 0, maxPasswords)
	add := func(password string) {
		password = strings.TrimSpace(password)
		if password == "" || found[password] {
			return
		}
		found[password] = true
		out = append(out, password)
	}

	for _, seed := range seeds {
		base := normalizeSeed(seed)
		if base == "" {
			continue
		}
		capitalized := capitalize(base)
		add(base)
		add(capitalized)
		add(base + year)
		add(capitalized + year)
		add(base + "!")
		add(capitalized + "!")
		add(base + "123")
		add(capitalized + "123")
		add(base + year + "!")
		add(capitalized + year + "!")
		add(base + "@" + year)
		add(capitalized + "@" + year)
	}
	for _, password := range DefaultPasswords() {
		add(password)
	}
	if len(out) > maxPasswords {
		out = out[:maxPasswords]
	}
	return out
}

// domainSuffixes are the last labels that make a seed a domain.
//
// A seed is usually a company, a product or a domain, and the useful part of
// a domain is its first label. The suffix list is what tells the two apart:
// "acme.example.com" is a domain and "john.smith" is a name.
var domainSuffixes = map[string]bool{
	"com": true, "net": true, "org": true, "io": true, "co": true,
	"uk": true, "de": true, "info": true, "biz": true, "dev": true,
	"app": true, "us": true, "ca": true, "au": true, "fr": true,
	"in": true, "jp": true, "nl": true, "se": true, "ch": true,
	"it": true, "es": true, "pl": true, "br": true, "za": true,
	"ai": true, "me": true, "tv": true,
}

// normalizeSeed reduces a seed to the name inside it.
//
// A seed is a company, a product or a domain, and the useful part of a
// domain is its first label.
func normalizeSeed(raw string) string {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return ""
	}
	if idx := strings.Index(trimmed, "@"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	if idx := strings.IndexAny(trimmed, "/?#"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	if idx := strings.Index(trimmed, ":"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	trimmed = strings.Trim(trimmed, ". ")
	trimmed = strings.ReplaceAll(trimmed, " ", "")
	if idx := strings.LastIndex(trimmed, "."); idx > 0 {
		last := trimmed[idx+1:]
		if domainSuffixes[last] {
			// A domain is reduced to its first label: "acme.example.com"
			// becomes "acme".
			if first := strings.Index(trimmed, "."); first > 0 {
				trimmed = trimmed[:first]
			}
		}
	}
	if len(trimmed) > maxSeedLength {
		trimmed = trimmed[:maxSeedLength]
	}
	return trimmed
}

// capitalize upper-cases the first letter, which is the difference between
// "acme2026" and the "Acme2026" that gets typed.
func capitalize(value string) string {
	if value == "" {
		return value
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

// DefaultPasswords returns the built-in password list.
//
// The list is the set of passwords that turn up in breach corpora and in the
// defaults people leave behind: the season and the year, the company name,
// the keyboard walk, the common defaults, the team names, the months and
// the classics. It is a word list, not an exploit, and it is exactly the
// list a professional tests first.
//
// The returned slice is a copy, so a caller that sorts or truncates it
// cannot change what the next caller gets.
func DefaultPasswords() []string {
	found := make(map[string]bool)
	out := make([]string, 0, 256)
	for _, line := range strings.Split(defaultPasswordsText, "\n") {
		entry := strings.TrimSpace(line)
		if entry == "" || strings.HasPrefix(entry, "#") || found[entry] {
			continue
		}
		found[entry] = true
		out = append(out, entry)
	}
	return out
}

// defaultPasswordsText is the embedded list, one entry per line. A line
// starting with # is a comment and is skipped by the same rule the file
// loader uses.
const defaultPasswordsText = `
# Keyboard walks and patterns
qwerty
qwerty1
qwerty12
qwerty123
qwertyuiop
asdfgh
asdfghjkl
zxcvbnm
1qaz2wsx
1q2w3e
1q2w3e4r
1q2w3e4r5t
qazwsx
qaz1zaq1
zaq12wsx
qwe123
qweasd
abcd1234
1234qwer
aaaaaa
abc12345
a1b2c3
111111
121212
654321
000000
666666
696969
112233
123321

# The common defaults
password
password1
password123
Password1
Password123
passw0rd
Passw0rd
p@ssw0rd
p@ssword
letmein
welcome
welcome1
Welcome1
welcome123
changeme
default
secret
secret1
admin
admin1
admin12
admin123
admin1234
administrator
root
root123
toor
user
guest
test
test123
pass
pass123
pass1234
login
master
access
hello
hello123
12345
123456
1234567
12345678
123456789
1234567890
000000
love
lover
lover1
iloveyou
sunshine
princess
flower
buttercup
bailey
phoenix
shadow
monkey
dragon
dragon1
ginger
pepper
cookie
chicken
cheese
coffee
chocolate
apple
orange
banana
ninja
hacker
starwars
matrix
freedom
whatever
nothing
god
sex
money
power
soccer
football
baseball
hockey
batman
superman
trustno1
michael
jordan
harley
ranger
daniel
andrew
tigger
jessica
hunter
buster
killer
george
joshua
charlie
silver
dallas
yankees
falcon
maggie
sparky
scooter
purple

# The seasons and the years
summer
winter
spring
autumn
fall
summer2023
summer2024
summer2025
summer2026
winter2023
winter2024
winter2025
winter2026
spring2024
spring2025
spring2026
autumn2024
autumn2025
autumn2026
fall2024
fall2025
fall2026
season2025
season2026

# The months and the years
january
february
march
april
may
june
july
august
september
october
november
december
monday
friday
january2024
january2025
january2026
february2025
february2026
march2025
march2026
april2025
april2026
may2025
may2026
june2025
june2026
july2025
july2026
august2025
august2026
september2025
september2026
october2025
october2026
november2025
november2026
december2025
december2026

# The company names
google
facebook
twitter
amazon
microsoft
netflix
tesla
samsung
sony
nokia
intel
oracle
cisco
dell
linkedin
spotify
uber
airbnb
paypal
starbucks
walmart
costco
nike
adidas
reebok
gucci
rolex
toyota
honda
ford
bmw
mercedes
porsche
yahoo
bing
windows
linux
ubuntu
redhat
steam
minecraft
fortnite
pokemon
dragonball
naruto
supermario
samsung2025
nike2025
apple2025

# The teams
arsenal
chelsea
liverpool
manchester
madrid
barcelona
juventus
celtics
rangers
lakers
bulls
cowboys
eagles
packers
warriors
patriots
dolphins
giants
redsox
`
