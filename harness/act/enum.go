package act

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// DomainInfo is what the rootDSE says.
//
// The rootDSE is the one part of a directory every server answers without a
// bind, so it is where a domain's own account of itself comes from: the
// naming contexts, the domain controller's name, and the functional levels
// that say which year of Windows this is.
type DomainInfo struct {
	NamingContexts                     []string
	DefaultNamingContext               string
	DNSHostName                        string
	DomainController                   string
	ForestFunctionalityLevel           int
	DomainControllerFunctionalityLevel int
	SupportedLDAPVersion               []string
	VendorName                         string
}

// The attributes worth reading, per object class.
//
// A request is a footprint as well as a question, and an enumeration that
// asks for everything asks for the password hash as well. These lists name
// the attributes a roast needs and nothing else.
var (
	rootDSEAttributes = []string{
		"namingContexts",
		"defaultNamingContext",
		"dnsHostName",
		"domainController",
		"forestFunctionalityLevel",
		"domainControllerFunctionalityLevel",
		"supportedLDAPVersion",
		"vendorName",
	}
	userAttributes = []string{
		"sAMAccountName",
		"userPrincipalName",
		"mail",
		"memberOf",
		"pwdLastSet",
		"accountExpires",
		"userAccountControl",
		"whenCreated",
		"lastLogonTimestamp",
	}
	groupAttributes = []string{
		"sAMAccountName",
		"memberOf",
		"userAccountControl",
		"whenCreated",
	}
	computerAttributes = []string{
		"sAMAccountName",
		"dnsHostName",
		"operatingSystem",
		"operatingSystemVersion",
		"userAccountControl",
		"whenCreated",
		"lastLogonTimestamp",
	}
	spnAttributes = []string{
		"sAMAccountName",
		"userPrincipalName",
		"servicePrincipalName",
		"pwdLastSet",
		"accountExpires",
		"userAccountControl",
		"whenCreated",
		"lastLogonTimestamp",
	}
	trustAttributes = []string{
		"sAMAccountName",
		"trustDirection",
		"trustAttributes",
		"whenCreated",
	}
)

// credentialAttributes never leave the directory.
//
// No enumeration here asks for a password hash, and the ones that arrive
// anyway are dropped rather than returned: a directory read that pulls
// hashes into a transcript is a liability, and the transcript is exactly
// where a liability ends up in the wrong hands.
var credentialAttributes = map[string]bool{
	"unicodepwd":               true,
	"ntpwdhistory":             true,
	"dbcspwd":                  true,
	"lmpwdhash":                true,
	"supplementalcredentials":  true,
	"unicodepwdhistory":        true,
	"msds-allowedtodelegateto": true,
}

// Info reads the rootDSE.
//
// The base is empty and the scope is base, which is how a rootDSE is read:
// it is the root of the tree and has no name.
func Info(ctx context.Context, c *Client) (DomainInfo, error) {
	entries, err := c.Search(ctx, "", ScopeBase, PresenceFilter("objectClass"), rootDSEAttributes, 1)
	if err != nil {
		return DomainInfo{}, err
	}
	if len(entries) == 0 {
		return DomainInfo{}, errors.New("act: the rootDSE returned no entry")
	}
	entry := entries[0]
	return DomainInfo{
		NamingContexts:                     entry.GetAll("namingContexts"),
		DefaultNamingContext:               entry.Get("defaultNamingContext"),
		DNSHostName:                        entry.Get("dnsHostName"),
		DomainController:                   entry.Get("domainController"),
		ForestFunctionalityLevel:           entry.GetInt("forestFunctionalityLevel"),
		DomainControllerFunctionalityLevel: entry.GetInt("domainControllerFunctionalityLevel"),
		SupportedLDAPVersion:               entry.GetAll("supportedLDAPVersion"),
		VendorName:                         entry.Get("vendorName"),
	}, nil
}

// Mode is one enumeration of the directory.
type Mode string

// The enumerations.
const (
	// ModeUsers is the user objects of a domain.
	ModeUsers Mode = "users"
	// ModeGroups is the group objects of a domain.
	ModeGroups Mode = "groups"
	// ModeComputers is the computer objects of a domain.
	ModeComputers Mode = "computers"
	// ModeSPN is the accounts with a service principal name.
	ModeSPN Mode = "spn"
	// ModeTrusts is the trust objects of a domain.
	ModeTrusts Mode = "trusts"
)

// SearchSpec is what one enumeration asks the directory for.
type SearchSpec struct {
	Base       string
	Scope      Scope
	Filter     Filter
	Attributes []string
}

// Spec returns the search one mode performs against a base.
//
// The modes and their filters live here so that a caller who wants to add a
// condition of its own does not have to repeat the mode's filter and get it
// subtly wrong.
func (m Mode) Spec(base string) (SearchSpec, error) {
	switch m {
	case ModeUsers:
		// A computer is a user in Active Directory: the computer class
		// inherits from the user class, so a filter of objectClass=user
		// alone returns the domain controllers as well. The explicit
		// exclusion is what keeps the two lists apart.
		return SearchSpec{
			Base:  base,
			Scope: ScopeSubtree,
			Filter: AndFilter(
				EqualityFilter("objectClass", "user"),
				NotFilter(EqualityFilter("objectClass", "computer")),
			),
			Attributes: userAttributes,
		}, nil
	case ModeGroups:
		return SearchSpec{
			Base:       base,
			Scope:      ScopeSubtree,
			Filter:     EqualityFilter("objectClass", "group"),
			Attributes: groupAttributes,
		}, nil
	case ModeComputers:
		return SearchSpec{
			Base:       base,
			Scope:      ScopeSubtree,
			Filter:     EqualityFilter("objectClass", "computer"),
			Attributes: computerAttributes,
		}, nil
	case ModeSPN:
		// A computer carries a service principal name too and is a user, so
		// this set covers both. It is the set that decides the next hour:
		// an account with an SPN can be asked for a ticket without a
		// password.
		return SearchSpec{
			Base:  base,
			Scope: ScopeSubtree,
			Filter: AndFilter(
				EqualityFilter("objectClass", "user"),
				PresenceFilter("servicePrincipalName"),
			),
			Attributes: spnAttributes,
		}, nil
	case ModeTrusts:
		return SearchSpec{
			Base:       base,
			Scope:      ScopeSubtree,
			Filter:     EqualityFilter("objectClass", "trustedDomain"),
			Attributes: trustAttributes,
		}, nil
	}
	return SearchSpec{}, fmt.Errorf("act: %q is not an enumeration", string(m))
}

// Users lists the user objects.
func Users(ctx context.Context, c *Client, domain string, limit int) ([]Entry, error) {
	return runMode(ctx, c, ModeUsers, domain, limit)
}

// Groups lists the group objects.
func Groups(ctx context.Context, c *Client, domain string, limit int) ([]Entry, error) {
	return runMode(ctx, c, ModeGroups, domain, limit)
}

// Computers lists the computer objects.
func Computers(ctx context.Context, c *Client, domain string, limit int) ([]Entry, error) {
	return runMode(ctx, c, ModeComputers, domain, limit)
}

// SPNAccounts lists the accounts with a servicePrincipalName, which are the
// roastable ones.
func SPNAccounts(ctx context.Context, c *Client, domain string, limit int) ([]Entry, error) {
	return runMode(ctx, c, ModeSPN, domain, limit)
}

// Trusts lists the trust objects.
func Trusts(ctx context.Context, c *Client, domain string, limit int) ([]Entry, error) {
	return runMode(ctx, c, ModeTrusts, domain, limit)
}

// runMode runs one enumeration and drops the attributes that must not be
// carried out of a directory.
func runMode(ctx context.Context, c *Client, mode Mode, domain string, limit int) ([]Entry, error) {
	base, err := BaseDN(domain)
	if err != nil {
		return nil, err
	}
	spec, err := mode.Spec(base)
	if err != nil {
		return nil, err
	}
	return searchAccounts(ctx, c, spec, limit)
}

// searchAccounts runs one search and drops the attributes that must not be
// carried out of a directory.
func searchAccounts(ctx context.Context, c *Client, spec SearchSpec, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = maxEntries
	}
	if limit > maxEntries {
		limit = maxEntries
	}
	entries, err := c.Search(ctx, spec.Base, spec.Scope, spec.Filter, spec.Attributes, limit)
	if err != nil {
		if errors.Is(err, ErrSizeLimit) {
			// The entries that were read are real observations, so they are
			// returned with the error rather than thrown away. The caller
			// sees both.
			return sanitizeEntries(entries), err
		}
		return nil, err
	}
	return sanitizeEntries(entries), nil
}

// sanitizeEntries drops the credential-bearing attributes from every entry.
func sanitizeEntries(entries []Entry) []Entry {
	for i := range entries {
		entries[i] = sanitizeEntry(entries[i])
	}
	return entries
}

// sanitizeEntry drops the credential-bearing attributes from one entry.
func sanitizeEntry(entry Entry) Entry {
	if len(entry.Attributes) == 0 {
		return entry
	}
	cleaned := make(map[string][]string, len(entry.Attributes))
	for name, values := range entry.Attributes {
		if credentialAttributes[strings.ToLower(name)] {
			continue
		}
		cleaned[name] = values
	}
	entry.Attributes = cleaned
	return entry
}

// BaseDN derives the base DN from a domain: example.com becomes
// DC=example,DC=com.
//
// A domain that is already a distinguished name is used as it stands,
// because the operator said so and a second reading of it would be a guess.
func BaseDN(domain string) (string, error) {
	trimmed := strings.TrimSpace(domain)
	if trimmed == "" {
		return "", errors.New("act: a domain is required")
	}
	if strings.Contains(trimmed, "=") {
		return trimmed, nil
	}
	trimmed = strings.TrimSuffix(trimmed, ".")
	parts := strings.Split(trimmed, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("act: %q is not a domain name", domain)
	}
	labels := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return "", fmt.Errorf("act: %q is not a domain name", domain)
		}
		labels = append(labels, "DC="+part)
	}
	return strings.Join(labels, ","), nil
}
