package act

import (
	"context"
	"strings"
	"testing"
	"time"
)

// searchRequest is what the test reads out of the request the client sent.
type searchRequest struct {
	Base       string
	Scope      Scope
	SizeLimit  int
	Filter     Filter
	Attributes []string
}

// enumResult is what the fake directory hands back: the request it read, or
// the reason it could not read it.
type enumResult struct {
	request searchRequest
	err     error
}

// decodeSearchRequest reads a search request out of the wire.
//
// It returns an error rather than failing the test, because it runs in the
// server's goroutine and a test that fails from another goroutine has
// already lost the thread it was on.
func decodeSearchRequest(request []byte) (searchRequest, error) {
	_, content, _, err := Decode(request)
	if err != nil {
		return searchRequest{}, err
	}
	_, _, rest, err := Decode(content)
	if err != nil {
		return searchRequest{}, err
	}
	tag, opContent, _, err := Decode(rest)
	if err != nil {
		return searchRequest{}, err
	}
	if tag != tagSearchRequest {
		return searchRequest{}, errorString("the request is not a search")
	}
	return decodeSearchBody(opContent)
}

// decodeSearchBody reads the fields of a search request.
func decodeSearchBody(content []byte) (searchRequest, error) {
	tag, baseContent, rest, err := Decode(content)
	if err != nil {
		return searchRequest{}, err
	}
	if tag != TagOctetString {
		return searchRequest{}, errorString("the base is not an octet string")
	}
	out := searchRequest{Base: DecodeString(baseContent)}

	tag, scopeContent, rest, err := Decode(rest)
	if err != nil {
		return searchRequest{}, err
	}
	if tag != TagEnumerated {
		return searchRequest{}, errorString("the scope is not an enumerated")
	}
	scope, err := DecodeInteger(scopeContent)
	if err != nil {
		return searchRequest{}, err
	}
	out.Scope = Scope(scope)

	// derefAliases
	_, _, rest, err = Decode(rest)
	if err != nil {
		return searchRequest{}, err
	}
	// sizeLimit
	tag, limitContent, rest, err := Decode(rest)
	if err != nil {
		return searchRequest{}, err
	}
	if tag != TagInteger {
		return searchRequest{}, errorString("the size limit is not an integer")
	}
	limit, err := DecodeInteger(limitContent)
	if err != nil {
		return searchRequest{}, err
	}
	out.SizeLimit = int(limit)
	// timeLimit
	_, _, rest, err = Decode(rest)
	if err != nil {
		return searchRequest{}, err
	}
	// typesOnly
	_, _, rest, err = Decode(rest)
	if err != nil {
		return searchRequest{}, err
	}
	// The filter.
	_, _, afterFilter, err := Decode(rest)
	if err != nil {
		return searchRequest{}, err
	}
	filterElement := rest[:len(rest)-len(afterFilter)]
	filter, _, err := decodeFilter(filterElement)
	if err != nil {
		return searchRequest{}, err
	}
	out.Filter = filter
	rest = afterFilter

	// The attributes.
	tag, attributesContent, _, err := Decode(rest)
	if err != nil {
		return searchRequest{}, err
	}
	if tag != TagSequence {
		return searchRequest{}, errorString("the attributes are not a sequence")
	}
	remaining := attributesContent
	for len(remaining) > 0 {
		tag, value, next, err := Decode(remaining)
		if err != nil {
			break
		}
		remaining = next
		if tag != TagOctetString {
			continue
		}
		out.Attributes = append(out.Attributes, DecodeString(value))
	}
	return out, nil
}

// decodeFilter reads a filter out of the wire.
func decodeFilter(content []byte) (Filter, []byte, error) {
	tag, body, rest, err := Decode(content)
	if err != nil {
		return nil, nil, err
	}
	switch tag {
	case tagFilterPresent:
		return PresenceFilter(DecodeString(body)), rest, nil
	case tagFilterEquality:
		_, attribute, after, err := Decode(body)
		if err != nil {
			return nil, nil, err
		}
		_, value, _, err := Decode(after)
		if err != nil {
			return nil, nil, err
		}
		return EqualityFilter(DecodeString(attribute), DecodeString(value)), rest, nil
	case tagFilterSubstrings:
		_, attribute, after, err := Decode(body)
		if err != nil {
			return nil, nil, err
		}
		_, substrings, _, err := Decode(after)
		if err != nil {
			return nil, nil, err
		}
		prefix, middle, suffix := "", "", ""
		remaining := substrings
		for len(remaining) > 0 {
			partTag, partContent, next, err := Decode(remaining)
			if err != nil {
				break
			}
			remaining = next
			switch partTag {
			case tagSubstringInitial:
				prefix = DecodeString(partContent)
			case tagSubstringAny:
				middle = DecodeString(partContent)
			case tagSubstringFinal:
				suffix = DecodeString(partContent)
			}
		}
		return SubstringFilter(DecodeString(attribute), prefix, middle, suffix), rest, nil
	case tagFilterAnd, tagFilterOr, tagFilterNot:
		var filters []Filter
		remaining := body
		for len(remaining) > 0 {
			filter, next, err := decodeFilter(remaining)
			if err != nil {
				return nil, nil, err
			}
			filters = append(filters, filter)
			remaining = next
		}
		switch tag {
		case tagFilterAnd:
			return AndFilter(filters...), rest, nil
		case tagFilterOr:
			return OrFilter(filters...), rest, nil
		default:
			if len(filters) != 1 {
				return nil, nil, errNotOneFilter
			}
			return NotFilter(filters[0]), rest, nil
		}
	}
	return nil, nil, errUnknownFilter
}

// The two errors the test's filter reader can raise.
var (
	errNotOneFilter  = errorString("the not filter does not carry one filter")
	errUnknownFilter = errorString("the filter is not one of the known forms")
)

// errorString is a small error type for the test.
type errorString string

func (e errorString) Error() string { return string(e) }

// ---------------------------------------------------------------------------
// The enumerations
// ---------------------------------------------------------------------------

// enumServer answers a search with one canned entry and records the request.
func enumServer(t *testing.T, seen chan enumResult, entry Entry) *fakeLDAP {
	t.Helper()
	openTheWall(t)
	return startFakeLDAP(t, func(request []byte) []byte {
		id := requestID(request)
		parsed, err := decodeSearchRequest(request)
		seen <- enumResult{request: parsed, err: err}
		attributes := map[string][]string{}
		for name, values := range entry.Attributes {
			attributes[name] = values
		}
		reply := ldapMessage(id, searchEntry(entry.DN, attributes))
		return append(reply, ldapMessage(id, searchDone(0))...)
	})
}

func TestEnumerationsPinTheFiltersAndTheAttributes(t *testing.T) {
	cases := []struct {
		name       string
		run        func(ctx context.Context, c *Client) ([]Entry, error)
		wantBase   string
		wantFilter string
		wantAttrs  []string
	}{
		{
			name: "users",
			run: func(ctx context.Context, c *Client) ([]Entry, error) {
				return Users(ctx, c, "example.com", 10)
			},
			wantBase:   "DC=example,DC=com",
			wantFilter: "(&(objectClass=user)(!(objectClass=computer)))",
			wantAttrs: []string{
				"sAMAccountName", "userPrincipalName", "mail", "memberOf",
				"pwdLastSet", "accountExpires", "userAccountControl",
				"whenCreated", "lastLogonTimestamp",
			},
		},
		{
			name: "groups",
			run: func(ctx context.Context, c *Client) ([]Entry, error) {
				return Groups(ctx, c, "example.com", 10)
			},
			wantBase:   "DC=example,DC=com",
			wantFilter: "(objectClass=group)",
			wantAttrs: []string{
				"sAMAccountName", "memberOf", "userAccountControl", "whenCreated",
			},
		},
		{
			name: "computers",
			run: func(ctx context.Context, c *Client) ([]Entry, error) {
				return Computers(ctx, c, "example.com", 10)
			},
			wantBase:   "DC=example,DC=com",
			wantFilter: "(objectClass=computer)",
			wantAttrs: []string{
				"sAMAccountName", "dnsHostName", "operatingSystem", "operatingSystemVersion",
				"userAccountControl", "whenCreated", "lastLogonTimestamp",
			},
		},
		{
			name: "spn",
			run: func(ctx context.Context, c *Client) ([]Entry, error) {
				return SPNAccounts(ctx, c, "example.com", 10)
			},
			wantBase:   "DC=example,DC=com",
			wantFilter: "(&(objectClass=user)(servicePrincipalName=*))",
			wantAttrs: []string{
				"sAMAccountName", "userPrincipalName", "servicePrincipalName",
				"pwdLastSet", "accountExpires", "userAccountControl",
				"whenCreated", "lastLogonTimestamp",
			},
		},
		{
			name: "trusts",
			run: func(ctx context.Context, c *Client) ([]Entry, error) {
				return Trusts(ctx, c, "example.com", 10)
			},
			wantBase:   "DC=example,DC=com",
			wantFilter: "(objectClass=trustedDomain)",
			wantAttrs: []string{
				"sAMAccountName", "trustDirection", "trustAttributes", "whenCreated",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seen := make(chan enumResult, 4)
			server := enumServer(t, seen, Entry{DN: "CN=alice,DC=example,DC=com", Attributes: map[string][]string{
				"sAMAccountName": {"alice"},
			}})
			client, err := Dial(context.Background(), "tcp", server.address(), 5*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer client.Close()
			if _, err := tc.run(context.Background(), client); err != nil {
				t.Fatalf("the enumeration failed: %v", err)
			}
			result := <-seen
			if result.err != nil {
				t.Fatalf("the request could not be read: %v", result.err)
			}
			request := result.request
			if request.Base != tc.wantBase {
				t.Errorf("the base is %q, want %q", request.Base, tc.wantBase)
			}
			if request.Scope != ScopeSubtree {
				t.Errorf("the scope is %d, want %d", request.Scope, ScopeSubtree)
			}
			if request.SizeLimit != 10 {
				t.Errorf("the size limit is %d, want 10", request.SizeLimit)
			}
			if got := filterToString(request.Filter); got != tc.wantFilter {
				t.Errorf("the filter is %s, want %s", got, tc.wantFilter)
			}
			if strings.Join(request.Attributes, ",") != strings.Join(tc.wantAttrs, ",") {
				t.Errorf("the attributes are %v, want %v", request.Attributes, tc.wantAttrs)
			}
		})
	}
}

func TestEnumerationDropsTheCredentialAttributes(t *testing.T) {
	seen := make(chan enumResult, 4)
	openTheWall(t)
	server := startFakeLDAP(t, func(request []byte) []byte {
		id := requestID(request)
		parsed, err := decodeSearchRequest(request)
		seen <- enumResult{request: parsed, err: err}
		reply := ldapMessage(id, searchEntry("CN=alice,DC=example,DC=com", map[string][]string{
			"sAMAccountName": {"alice"},
			"unicodePwd":     {"a password hash that must not travel"},
		}))
		return append(reply, ldapMessage(id, searchDone(0))...)
	})
	client, err := Dial(context.Background(), "tcp", server.address(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	entries, err := Users(context.Background(), client, "example.com", 10)
	if err != nil {
		t.Fatalf("the enumeration failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d entries, want 1", len(entries))
	}
	if len(entries[0].Attributes["unicodePwd"]) != 0 {
		t.Error("a password hash was carried out of the directory")
	}
	if entries[0].Get("sAMAccountName") != "alice" {
		t.Error("the entry lost the attribute it was asked for")
	}
}

func TestBaseDNDerivesFromTheDomain(t *testing.T) {
	got, err := BaseDN("example.com")
	if err != nil {
		t.Fatalf("BaseDN: %v", err)
	}
	if got != "DC=example,DC=com" {
		t.Errorf("BaseDN(example.com) = %q, want DC=example,DC=com", got)
	}
	got, err = BaseDN("sub.example.co.uk.")
	if err != nil {
		t.Fatalf("BaseDN: %v", err)
	}
	if got != "DC=sub,DC=example,DC=co,DC=uk" {
		t.Errorf("BaseDN = %q, want DC=sub,DC=example,DC=co,DC=uk", got)
	}
	got, err = BaseDN("DC=example,DC=com")
	if err != nil {
		t.Fatalf("BaseDN: %v", err)
	}
	if got != "DC=example,DC=com" {
		t.Errorf("a base DN that was already one was rewritten: %q", got)
	}
	if _, err := BaseDN(""); err == nil {
		t.Error("an empty domain was accepted")
	}
	if _, err := BaseDN("example"); err == nil {
		t.Error("a single label was accepted as a domain")
	}
}

func TestInfoReadsTheRootDSE(t *testing.T) {
	openTheWall(t)
	server := startFakeLDAP(t, func(request []byte) []byte {
		id := requestID(request)
		reply := ldapMessage(id, searchEntry("", map[string][]string{
			"namingContexts":                     {"DC=example,DC=com", "CN=Configuration,DC=example,DC=com"},
			"defaultNamingContext":               {"DC=example,DC=com"},
			"dnsHostName":                        {"dc01.example.com"},
			"domainController":                   {"dc01.example.com"},
			"forestFunctionalityLevel":           {"7"},
			"domainControllerFunctionalityLevel": {"7"},
			"supportedLDAPVersion":               {"3"},
			"vendorName":                         {"Microsoft Corporation"},
		}))
		return append(reply, ldapMessage(id, searchDone(0))...)
	})
	client, err := Dial(context.Background(), "tcp", server.address(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	info, err := Info(context.Background(), client)
	if err != nil {
		t.Fatalf("the rootDSE read failed: %v", err)
	}
	if info.DefaultNamingContext != "DC=example,DC=com" {
		t.Errorf("the default naming context is %q", info.DefaultNamingContext)
	}
	if len(info.NamingContexts) != 2 {
		t.Errorf("%d naming contexts, want 2", len(info.NamingContexts))
	}
	if info.ForestFunctionalityLevel != 7 {
		t.Errorf("the forest functionality level is %d, want 7", info.ForestFunctionalityLevel)
	}
	if info.VendorName != "Microsoft Corporation" {
		t.Errorf("the vendor is %q", info.VendorName)
	}
}
