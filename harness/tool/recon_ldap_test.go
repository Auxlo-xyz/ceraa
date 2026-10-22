package tool

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"ceraa/harness/act"
	"ceraa/harness/netguard"
	"ceraa/harness/operation"
	"ceraa/harness/recon"
)

// The fake directory the tool talks to.
//
// The tool dials for itself, so the test needs a real server on the
// loopback interface, and the engagement record has to name it: the wall
// refuses loopback, and the engagement is how an operator opens a range.
type fakeDirectory struct {
	listener net.Listener
}

func startFakeDirectory(t *testing.T, handler func(request []byte) []byte) *fakeDirectory {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &fakeDirectory{listener: listener}
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

func (s *fakeDirectory) address() string {
	return s.listener.Addr().String()
}

func (s *fakeDirectory) serve(conn net.Conn, handler func([]byte) []byte) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		request, err := readFakeElement(reader)
		if err != nil {
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

func readFakeElement(reader *bufio.Reader) ([]byte, error) {
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

// The bytes of an LDAP message, built by hand so the test does not depend on
// the encoder it is testing.
func fakeMessage(id int, op []byte) []byte {
	content := append([]byte{0x02, 0x01, byte(id)}, op...)
	out := []byte{0x30, byte(len(content))}
	return append(out, content...)
}

func fakeBindResponse(id int, code int) []byte {
	result := []byte{0x0a, 0x01, byte(code), 0x04, 0x00, 0x04, 0x00}
	return fakeMessage(id, append([]byte{0x61, byte(len(result))}, result...))
}

func fakeSearchEntry(id int, dn string, name string, value string) []byte {
	// One attribute is a sequence of the name and the set of values.
	values := []byte{0x04, byte(len(value))}
	values = append(values, value...)
	values = append([]byte{0x31, byte(len(values))}, values...)
	attribute := []byte{0x04, byte(len(name))}
	attribute = append(attribute, name...)
	attribute = append(attribute, values...)
	attribute = append([]byte{0x30, byte(len(attribute))}, attribute...)
	// The attribute list is a sequence of those.
	attributes := append([]byte{0x30, byte(len(attribute))}, attribute...)
	// The entry is the name and the attribute list.
	body := []byte{0x04, byte(len(dn))}
	body = append(body, dn...)
	body = append(body, attributes...)
	return fakeMessage(id, append([]byte{0x64, byte(len(body))}, body...))
}

func fakeSearchDone(id int) []byte {
	result := []byte{0x0a, 0x01, 0x00, 0x04, 0x00, 0x04, 0x00}
	return fakeMessage(id, append([]byte{0x65, byte(len(result))}, result...))
}

// requestMessageID reads the message id out of a request.
func requestMessageID(request []byte) int {
	// The message is 30 <length> 02 01 <id> ...
	if len(request) < 5 || request[0] != 0x30 {
		return 0
	}
	offset := 2
	if request[1]&0x80 != 0 {
		offset = 2 + int(request[1]&0x7f)
	}
	if len(request) < offset+3 || request[offset] != 0x02 {
		return 0
	}
	return int(request[offset+2])
}

// requestOpTag reads the protocol operation tag out of a request.
func requestOpTag(request []byte) byte {
	if len(request) < 4 || request[0] != 0x30 {
		return 0
	}
	offset := 2
	if request[1]&0x80 != 0 {
		offset = 2 + int(request[1]&0x7f)
	}
	// The message id is three octets: the tag, the length and the value.
	if len(request) <= offset+3 {
		return 0
	}
	return request[offset+3]
}

// openTheTestEngagement names the loopback directory the test talks to.
func openTheTestEngagement(t *testing.T) {
	t.Helper()
	t.Setenv("CERAA_HOME", t.TempDir())
	if err := recon.SetActive(recon.Engagement{
		InScope:      []string{"127.0.0.1"},
		AllowPrivate: []string{"127.0.0.1"},
	}); err != nil {
		t.Fatalf("set the engagement: %v", err)
	}
	netguard.SetAuthorizedRanges([]string{"127.0.0.1"})
	t.Cleanup(func() {
		recon.Stop()
		netguard.SetAuthorizedRanges(nil)
	})
}

func TestTheLDAPToolNeedsAHost(t *testing.T) {
	tool := &LDAPTool{}
	if _, err := tool.Translate(map[string]any{}); err == nil {
		t.Fatal("a call with no host was accepted")
	}
	if _, err := tool.Translate(map[string]any{"host": "   "}); err == nil {
		t.Fatal("a call with a blank host was accepted")
	}
}

func TestTheLDAPToolRefusesAModeItDoesNotHave(t *testing.T) {
	tool := &LDAPTool{}
	_, err := tool.Translate(map[string]any{"host": "dc01.example.com", "mode": "everything"})
	if err == nil {
		t.Fatal("an unknown mode was accepted")
	}
	if !strings.Contains(err.Error(), "rootdse") {
		t.Errorf("the refusal does not name the modes: %v", err)
	}
}

func TestTheLDAPToolRefusesASchemeItDoesNotHave(t *testing.T) {
	tool := &LDAPTool{}
	if _, err := tool.Translate(map[string]any{"host": "dc01.example.com", "scheme": "ldaps-plus"}); err == nil {
		t.Fatal("an unknown scheme was accepted")
	}
}

func TestTheLDAPToolRefusesAFilterItCannotHonour(t *testing.T) {
	tool := &LDAPTool{}
	_, err := tool.Translate(map[string]any{"host": "dc01.example.com", "filter": "(cn>=x)"})
	if err == nil {
		t.Fatal("a filter the parser cannot honour was accepted")
	}
}

func TestTheLDAPToolDefaults(t *testing.T) {
	tool := &LDAPTool{}
	spec, err := tool.Translate(map[string]any{"host": "dc01.example.com"})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if spec.Type != "ldap" {
		t.Errorf("the operation type is %q, want ldap", spec.Type)
	}
	if spec.Args["mode"] != "rootdse" {
		t.Errorf("the mode is %v, want rootdse", spec.Args["mode"])
	}
	if spec.Args["port"] != 389 {
		t.Errorf("the port is %v, want 389", spec.Args["port"])
	}
	if spec.Args["scheme"] != "ldap" {
		t.Errorf("the scheme is %v, want ldap", spec.Args["scheme"])
	}
	spec, err = tool.Translate(map[string]any{"host": "dc01.example.com", "scheme": "ldaps"})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if spec.Args["port"] != 636 {
		t.Errorf("the ldaps port is %v, want 636", spec.Args["port"])
	}
}

func TestTheLDAPToolNeverPrintsTheBindPassword(t *testing.T) {
	openTheTestEngagement(t)
	directory := startFakeDirectory(t, func(request []byte) []byte {
		id := requestMessageID(request)
		if requestOpTag(request) == 0x60 {
			return fakeBindResponse(id, 0)
		}
		reply := fakeSearchEntry(id, "", "namingContexts", "DC=example,DC=com")
		return append(reply, fakeSearchDone(id)...)
	})
	tool := &LDAPTool{}
	spec, err := tool.Translate(map[string]any{
		"host":          "127.0.0.1",
		"port":          portOf(directory.address()),
		"bind_dn":       "CN=alice,DC=example,DC=com",
		"bind_password": "correct horse battery staple",
		"mode":          "rootdse",
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	output, err := tool.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(output, "correct horse") {
		t.Errorf("the output carries the bind password:\n%s", output)
	}
}

func TestTheLDAPToolRefusesAHostTheEngagementDoesNotName(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	recon.Stop()
	netguard.SetAuthorizedRanges(nil)
	t.Cleanup(func() { netguard.SetAuthorizedRanges(nil) })
	tool := &LDAPTool{}
	spec, err := tool.Translate(map[string]any{"host": "dc01.example.com"})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if _, err := tool.RunContext(context.Background(), spec); err == nil {
		t.Fatal("a host the engagement does not name was contacted")
	}
}

func TestTheLDAPToolRendersTheEntriesItRead(t *testing.T) {
	openTheTestEngagement(t)
	directory := startFakeDirectory(t, func(request []byte) []byte {
		id := requestMessageID(request)
		reply := fakeSearchEntry(id, "CN=alice,DC=example,DC=com", "sAMAccountName", "alice")
		return append(reply, fakeSearchDone(id)...)
	})
	tool := &LDAPTool{}
	spec, err := tool.Translate(map[string]any{
		"host":   "127.0.0.1",
		"port":   portOf(directory.address()),
		"mode":   "users",
		"domain": "example.com",
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	output, err := tool.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(output, "CN=alice,DC=example,DC=com") {
		t.Errorf("the output does not name the entry:\n%s", output)
	}
	if !strings.Contains(output, "sAMAccountName: alice") {
		t.Errorf("the output does not carry the attribute:\n%s", output)
	}
	if !strings.Contains(output, "DC=example,DC=com") {
		t.Errorf("the output does not name the base:\n%s", output)
	}
}

// portOf reads the port out of a host:port pair.
func portOf(address string) int {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return 389
	}
	number := 0
	for _, r := range port {
		if r < '0' || r > '9' {
			return 389
		}
		number = number*10 + int(r-'0')
	}
	return number
}

func TestTheLDAPToolIsRegisteredUnderItsOperationType(t *testing.T) {
	tool := &LDAPTool{}
	if tool.Name() != "ldap" {
		t.Errorf("the name is %q, want ldap", tool.Name())
	}
	spec, err := tool.Translate(map[string]any{"host": "dc01.example.com"})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if spec.Type != tool.Name() {
		t.Errorf("the operation type is %q and the name is %q, and the registry matches them by name", spec.Type, tool.Name())
	}
	var _ operation.Spec = spec
	_ = act.Entry{}
	_ = time.Second
}
