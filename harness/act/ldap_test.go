package act

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"ceraa/harness/netguard"
	"ceraa/harness/recon"
)

// ---------------------------------------------------------------------------
// A fake directory
// ---------------------------------------------------------------------------

// fakeLDAP is a directory that answers whatever the test tells it to.
type fakeLDAP struct {
	listener net.Listener
}

// startFakeLDAP runs a directory on the loopback interface.
//
// The wall refuses loopback, so the test opens it: the test is the operator
// here, and the engagement record is how an operator opens a range.
func startFakeLDAP(t *testing.T, handler func(request []byte) []byte) *fakeLDAP {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &fakeLDAP{listener: listener}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go server.serve(conn, handler)
		}
	}()
	t.Cleanup(func() { listener.Close() })
	return server
}

// address is where the fake directory is listening.
func (s *fakeLDAP) address() string {
	return s.listener.Addr().String()
}

// serve answers one request at a time.
func (s *fakeLDAP) serve(conn net.Conn, handler func([]byte) []byte) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		request, err := readTestElement(reader)
		if err != nil {
			return
		}
		// The unbind is the client's way of saying goodbye, and it deserves
		// no answer.
		if requestOpTag(request) == tagUnbindRequest {
			return
		}
		reply := handler(request)
		if reply == nil {
			return
		}
		if _, err := conn.Write(reply); err != nil {
			return
		}
	}
}

// requestOpTag reads the protocol operation tag out of a request.
func requestOpTag(request []byte) Tag {
	_, content, _, err := Decode(request)
	if err != nil {
		return 0
	}
	_, _, rest, err := Decode(content)
	if err != nil {
		return 0
	}
	tag, _, _, err := Decode(rest)
	if err != nil {
		return 0
	}
	return tag
}

// readTestElement reads one complete BER element.
func readTestElement(reader *bufio.Reader) ([]byte, error) {
	header := make([]byte, 0, 8)
	first, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	header = append(header, first)
	lengthByte, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	header = append(header, lengthByte)
	length := 0
	switch {
	case lengthByte == 0x80:
		return nil, errors.New("the indefinite length is not supported")
	case lengthByte < 0x80:
		length = int(lengthByte)
	default:
		count := int(lengthByte & 0x7f)
		extra := make([]byte, count)
		if _, err := io.ReadFull(reader, extra); err != nil {
			return nil, err
		}
		header = append(header, extra...)
		for _, b := range extra {
			length = length<<8 | int(b)
		}
	}
	content := make([]byte, length)
	if _, err := io.ReadFull(reader, content); err != nil {
		return nil, err
	}
	return append(header, content...), nil
}

// ldapMessage wraps a protocol operation in an LDAPMessage.
func ldapMessage(id int, op []byte) []byte {
	content := append(EncodeElement(TagInteger, encodeIntegerContent(int64(id))), op...)
	return EncodeElement(TagSequence, content)
}

// requestID reads the message id out of a request.
func requestID(request []byte) int {
	_, content, _, err := Decode(request)
	if err != nil {
		return 0
	}
	_, idContent, _, err := Decode(content)
	if err != nil {
		return 0
	}
	id, err := DecodeInteger(idContent)
	if err != nil {
		return 0
	}
	return int(id)
}

// ldapResult builds the result of a bind or a search.
func ldapResult(code int, diagnostic string) []byte {
	content := append(EncodeElement(TagEnumerated, encodeIntegerContent(int64(code))), EncodeElement(TagOctetString, nil)...)
	content = append(content, EncodeElement(TagOctetString, []byte(diagnostic))...)
	return content
}

// bindResponse builds a bind response.
func bindResponse(code int, diagnostic string) []byte {
	return EncodeElement(tagBindResponse, ldapResult(code, diagnostic))
}

// searchDone builds the end of a search.
func searchDone(code int) []byte {
	return EncodeElement(tagSearchResultDone, ldapResult(code, ""))
}

// searchEntry builds one search result.
func searchEntry(dn string, attributes map[string][]string) []byte {
	collected := make([]byte, 0, 128)
	for name, values := range attributes {
		set := make([]byte, 0, 64)
		for _, value := range values {
			set = append(set, EncodeElement(TagOctetString, []byte(value))...)
		}
		attribute := append(EncodeElement(TagOctetString, []byte(name)), EncodeElement(TagSet, set)...)
		collected = append(collected, EncodeElement(TagSequence, attribute)...)
	}
	content := append(EncodeElement(TagOctetString, []byte(dn)), EncodeElement(TagSequence, collected)...)
	return EncodeElement(tagSearchResultEntry, content)
}

// openTheWall lets the test reach its own loopback server.
func openTheWall(t *testing.T) {
	t.Helper()
	netguard.SetAuthorizedRanges([]string{"127.0.0.1"})
	t.Cleanup(func() { netguard.SetAuthorizedRanges(nil) })
}

// setEngagement installs an engagement for the test.
func setEngagement(t *testing.T, engagement recon.Engagement) {
	t.Helper()
	t.Setenv("CERAA_HOME", t.TempDir())
	if err := recon.SetActive(engagement); err != nil {
		t.Fatalf("set the engagement: %v", err)
	}
	t.Cleanup(func() { recon.Stop() })
}

// ---------------------------------------------------------------------------
// The bind
// ---------------------------------------------------------------------------

func TestBindSucceedsAndThePasswordNeverAppearsInTheError(t *testing.T) {
	openTheWall(t)
	server := startFakeLDAP(t, func(request []byte) []byte {
		return ldapMessage(requestID(request), bindResponse(0, ""))
	})
	client, err := Dial(context.Background(), "tcp", server.address(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	if err := client.Bind(context.Background(), "CN=alice,DC=example,DC=com", "s3cret"); err != nil {
		t.Fatalf("bind: %v", err)
	}
}

func TestBindRefusedNamesTheResultCode(t *testing.T) {
	openTheWall(t)
	server := startFakeLDAP(t, func(request []byte) []byte {
		return ldapMessage(requestID(request), bindResponse(49, "80090308: LdapErr"))
	})
	client, err := Dial(context.Background(), "tcp", server.address(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	err = client.Bind(context.Background(), "CN=alice,DC=example,DC=com", "s3cret")
	if err == nil {
		t.Fatal("a refused bind was reported as a success")
	}
	if !strings.Contains(err.Error(), "invalid credentials") {
		t.Errorf("the refusal does not read as words: %v", err)
	}
	if !strings.Contains(err.Error(), "49") {
		t.Errorf("the refusal does not name the result code: %v", err)
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("the refusal carries the password: %v", err)
	}
}

func TestBindRefusedWithInsufficientAccessReadsAsWords(t *testing.T) {
	openTheWall(t)
	server := startFakeLDAP(t, func(request []byte) []byte {
		return ldapMessage(requestID(request), bindResponse(50, ""))
	})
	client, err := Dial(context.Background(), "tcp", server.address(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	err = client.Bind(context.Background(), "CN=alice,DC=example,DC=com", "s3cret")
	if err == nil {
		t.Fatal("a refused bind was reported as a success")
	}
	if !strings.Contains(err.Error(), "insufficient access") {
		t.Errorf("the refusal does not read as words: %v", err)
	}
}

func TestBindWithoutANameButWithAPasswordIsRefusedBeforeTheWire(t *testing.T) {
	openTheWall(t)
	seen := make(chan []byte, 4)
	server := startFakeLDAP(t, func(request []byte) []byte {
		seen <- request
		return nil
	})
	client, err := Dial(context.Background(), "tcp", server.address(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	if err := client.Bind(context.Background(), "", "s3cret"); err == nil {
		t.Fatal("a password with no name was accepted")
	}
	select {
	case request := <-seen:
		t.Errorf("a request went out with a password and no name: % x", request)
	default:
	}
}

// ---------------------------------------------------------------------------
// The search
// ---------------------------------------------------------------------------

func TestSearchParsesTheEntries(t *testing.T) {
	openTheWall(t)
	server := startFakeLDAP(t, func(request []byte) []byte {
		id := requestID(request)
		reply := ldapMessage(id, searchEntry("CN=alice,DC=example,DC=com", map[string][]string{
			"sAMAccountName":    {"alice"},
			"userPrincipalName": {"alice@example.com"},
		}))
		reply = append(reply, ldapMessage(id, searchEntry("CN=bob,DC=example,DC=com", map[string][]string{
			"sAMAccountName": {"bob"},
		}))...)
		return append(reply, ldapMessage(id, searchDone(0))...)
	})
	client, err := Dial(context.Background(), "tcp", server.address(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	entries, err := client.Search(context.Background(), "DC=example,DC=com", ScopeSubtree, PresenceFilter("objectClass"), []string{"sAMAccountName"}, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("%d entries, want 2", len(entries))
	}
	if entries[0].DN != "CN=alice,DC=example,DC=com" {
		t.Errorf("the first DN is %q", entries[0].DN)
	}
	if got := entries[0].Get("samaccountname"); got != "alice" {
		t.Errorf("sAMAccountName = %q, want alice", got)
	}
	if got := entries[0].Get("userPrincipalName"); got != "alice@example.com" {
		t.Errorf("userPrincipalName = %q, want alice@example.com", got)
	}
	if got := entries[1].Get("sAMAccountName"); got != "bob" {
		t.Errorf("sAMAccountName = %q, want bob", got)
	}
}

func TestSearchStopsAtTheSizeLimit(t *testing.T) {
	openTheWall(t)
	server := startFakeLDAP(t, func(request []byte) []byte {
		id := requestID(request)
		reply := ldapMessage(id, searchEntry("CN=one,DC=example,DC=com", map[string][]string{"sAMAccountName": {"one"}}))
		reply = append(reply, ldapMessage(id, searchEntry("CN=two,DC=example,DC=com", map[string][]string{"sAMAccountName": {"two"}}))...)
		reply = append(reply, ldapMessage(id, searchEntry("CN=three,DC=example,DC=com", map[string][]string{"sAMAccountName": {"three"}}))...)
		return append(reply, ldapMessage(id, searchDone(0))...)
	})
	client, err := Dial(context.Background(), "tcp", server.address(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	entries, err := client.Search(context.Background(), "DC=example,DC=com", ScopeSubtree, PresenceFilter("objectClass"), []string{"sAMAccountName"}, 2)
	if !errors.Is(err, ErrSizeLimit) {
		t.Fatalf("the search did not report the size limit: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("%d entries returned, want 2", len(entries))
	}
}

func TestSearchReportsTheServersOwnSizeLimit(t *testing.T) {
	openTheWall(t)
	server := startFakeLDAP(t, func(request []byte) []byte {
		id := requestID(request)
		reply := ldapMessage(id, searchEntry("CN=one,DC=example,DC=com", map[string][]string{"sAMAccountName": {"one"}}))
		return append(reply, ldapMessage(id, searchDone(4))...)
	})
	client, err := Dial(context.Background(), "tcp", server.address(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	_, err = client.Search(context.Background(), "DC=example,DC=com", ScopeSubtree, PresenceFilter("objectClass"), []string{"sAMAccountName"}, 1)
	if !errors.Is(err, ErrSizeLimit) {
		t.Fatalf("the search did not report the size limit: %v", err)
	}
}

func TestSearchRefusesAnUnreadableResult(t *testing.T) {
	openTheWall(t)
	server := startFakeLDAP(t, func(request []byte) []byte {
		return ldapMessage(requestID(request), EncodeElement(tagSearchResultDone, []byte{0x09, 0x01, 0x05}))
	})
	client, err := Dial(context.Background(), "tcp", server.address(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	_, err = client.Search(context.Background(), "DC=example,DC=com", ScopeSubtree, PresenceFilter("objectClass"), nil, 1)
	if err == nil {
		t.Fatal("a malformed result was accepted")
	}
}

// ---------------------------------------------------------------------------
// The wall
// ---------------------------------------------------------------------------

func TestDialRefusesALoopbackHostTheWallHasNotOpened(t *testing.T) {
	netguard.SetAuthorizedRanges(nil)
	server := startFakeLDAP(t, func(request []byte) []byte { return nil })
	_, err := Dial(context.Background(), "tcp", server.address(), 5*time.Second)
	if err == nil {
		t.Fatal("the wall let a loopback connection through")
	}
	if !errors.Is(err, netguard.ErrBlocked) {
		t.Errorf("the refusal is not the wall's own: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The filters
// ---------------------------------------------------------------------------

func TestFilterEncodingIsPinned(t *testing.T) {
	cases := []struct {
		name   string
		filter Filter
		want   string
	}{
		{"equality", EqualityFilter("a", "b"), "a306040161040162"},
		{"presence", PresenceFilter("a"), "870161"},
		{"and", AndFilter(EqualityFilter("a", "b")), "a008a306040161040162"},
		{"or", OrFilter(EqualityFilter("a", "b")), "a108a306040161040162"},
		{"not", NotFilter(EqualityFilter("a", "b")), "a208a306040161040162"},
		{"substring", SubstringFilter("a", "x", "y", "z"), "a40e040161300980017881017982017a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := encodeFilter(tc.filter)
			if hexOf(got) != tc.want {
				t.Errorf("the filter encoded as %s, want %s", hexOf(got), tc.want)
			}
		})
	}
}

// hexOf renders bytes for a test failure message.
func hexOf(data []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(data)*2)
	for _, b := range data {
		out = append(out, digits[b>>4], digits[b&0x0f])
	}
	return string(out)
}

func TestParseFilterReadsTheCommonForms(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"(objectClass=user)", "(objectClass=user)"},
		{"(&(objectClass=user)(!(lockedOut=TRUE)))", "(&(objectClass=user)(!(lockedOut=TRUE)))"},
		{"(cn=*)", "(cn=*)"},
		{"(cn=svc*sql*)", "(cn=svc*sql*)"},
	}
	for _, tc := range cases {
		filter, err := ParseFilter(tc.text)
		if err != nil {
			t.Fatalf("ParseFilter(%q): %v", tc.text, err)
		}
		got := filterToString(filter)
		if got != tc.want {
			t.Errorf("ParseFilter(%q) = %s, want %s", tc.text, got, tc.want)
		}
	}
}

func TestParseFilterRefusesWhatItCannotHonour(t *testing.T) {
	for _, text := range []string{"", "objectClass=user", "(cn>=x)", "(cn=x))", "(&)"} {
		if _, err := ParseFilter(text); err == nil {
			t.Errorf("ParseFilter(%q) was accepted", text)
		}
	}
}

// filterToString renders a filter the way it is written, for a test to pin.
func filterToString(filter Filter) string {
	switch typed := filter.(type) {
	case equalityFilter:
		return "(" + typed.attribute + "=" + typed.value + ")"
	case presenceFilter:
		return "(" + typed.attribute + "=*)"
	case substringFilter:
		return "(" + typed.attribute + "=" + typed.prefix + "*" + typed.middle + "*" + typed.suffix + ")"
	case andFilter:
		out := "(&"
		for _, f := range typed.filters {
			out += filterToString(f)
		}
		return out + ")"
	case orFilter:
		out := "(|"
		for _, f := range typed.filters {
			out += filterToString(f)
		}
		return out + ")"
	case notFilter:
		return "(!" + filterToString(typed.filter) + ")"
	}
	return "(?)"
}

// ---------------------------------------------------------------------------
// The scope
// ---------------------------------------------------------------------------

func TestAuthorizeHostRefusesWhenNoEngagementIsOnRecord(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	recon.Stop()
	err := AuthorizeHost("dc01.example.com", 389)
	if err == nil {
		t.Fatal("a host was authorized with no engagement on record")
	}
	if !errors.Is(err, ErrNotAuthorized) {
		t.Errorf("the refusal is not the authorization error: %v", err)
	}
	if !strings.Contains(err.Error(), "dc01.example.com") {
		t.Errorf("the refusal does not name the host: %v", err)
	}
}

func TestAuthorizeHostFollowsTheEngagement(t *testing.T) {
	setEngagement(t, recon.Engagement{
		InScope:      []string{"dc01.example.com", "other.example.com:636"},
		AllowPrivate: []string{"127.0.0.1"},
	})
	if err := AuthorizeHost("dc01.example.com", 389); err != nil {
		t.Errorf("an in-scope host was refused: %v", err)
	}
	if err := AuthorizeHost("sub.dc01.example.com", 389); err != nil {
		t.Errorf("a subdomain of an in-scope host was refused: %v", err)
	}
	if err := AuthorizeHost("other.example.com", 636); err != nil {
		t.Errorf("an in-scope host and port were refused: %v", err)
	}
	if err := AuthorizeHost("other.example.com", 389); err == nil {
		t.Error("a host was authorized on a port its token did not name")
	}
	if err := AuthorizeHost("stranger.example.net", 389); err == nil {
		t.Error("a host the engagement does not name was authorized")
	}
}

func TestAuthorizeHostLetsTheOutOfScopeListWin(t *testing.T) {
	setEngagement(t, recon.Engagement{
		InScope:      []string{"dc01.example.com"},
		OutScope:     []string{"dc01.example.com:389"},
		AllowPrivate: []string{"127.0.0.1"},
	})
	if err := AuthorizeHost("dc01.example.com", 389); err == nil {
		t.Fatal("a host on the out-of-scope list was authorized")
	}
	if err := AuthorizeHost("dc01.example.com", 636); err != nil {
		t.Errorf("a port the out-of-scope list does not name was refused: %v", err)
	}
}
