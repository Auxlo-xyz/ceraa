package tool

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"ceraa/harness/enum"
	"ceraa/harness/operation"
)

// enumFakeTransport answers the enum sources from a table, so the tool suite
// never leaves the machine.
type enumFakeTransport struct {
	keys     []string
	answers  map[string]string
	statuses map[string]int
	requests []string
}

// RoundTrip answers from the table.
func (f *enumFakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.requests = append(f.requests, req.URL.String())
	for _, key := range f.keys {
		if !strings.Contains(req.URL.String(), key) {
			continue
		}
		status := http.StatusOK
		if code, ok := f.statuses[key]; ok {
			status = code
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(f.answers[key])),
			Request:    req,
		}, nil
	}
	return nil, fmt.Errorf("enum fake transport: no answer for %s", req.URL.String())
}

// enumFakeResolver answers DNS queries from a table.
type enumFakeResolver struct {
	addresses []net.IPAddr
	txt       []string
}

func (f *enumFakeResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f.addresses, nil
}

func (f *enumFakeResolver) LookupCNAME(ctx context.Context, host string) (string, error) {
	return "", &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func (f *enumFakeResolver) LookupMX(ctx context.Context, host string) ([]*net.MX, error) {
	return nil, nil
}

func (f *enumFakeResolver) LookupNS(ctx context.Context, host string) ([]*net.NS, error) {
	return nil, nil
}

func (f *enumFakeResolver) LookupTXT(ctx context.Context, host string) ([]string, error) {
	return f.txt, nil
}

// enumTool builds the tool with the seams filled.
func enumTool(t *testing.T, transport *enumFakeTransport, resolver enum.Resolver) *EnumTool {
	t.Helper()
	// The scope gate reads the engagement record from the data directory.
	// Pointing it at a temporary one keeps a record left on the machine from
	// refusing the fake sources.
	t.Setenv("CERAA_HOME", t.TempDir())
	return &EnumTool{Options: enum.Options{Transport: transport, Resolver: resolver, RetryBackoff: time.Millisecond}}
}

// TestEnumName pins the registry name, which is also the operation type.
func TestEnumName(t *testing.T) {
	tl := &EnumTool{}
	if tl.Name() != "enum" {
		t.Errorf("Name = %q, want enum", tl.Name())
	}
	spec, err := tl.Translate(map[string]any{"mode": "subdomains", "domain": "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Type != tl.Name() {
		t.Errorf("spec.Type = %q and Name = %q; the runner resolves the executor by the type", spec.Type, tl.Name())
	}
}

// TestEnumDescriptionNamesTheModes: the model reads this, so it has to say
// what each mode collects and where the data comes from.
func TestEnumDescriptionNamesTheModes(t *testing.T) {
	description := (&EnumTool{}).Description()
	for _, want := range []string{"subdomains", "dns", "host", "crt.sh", "Cert Spotter", "rdap.org", "stat.ripe.net"} {
		if !strings.Contains(description, want) {
			t.Errorf("the description does not mention %q: %s", want, description)
		}
	}
}

// TestEnumSchemaDeclaresTheArguments: a property the tool reads but the
// schema does not declare is refused by the harness before the tool runs.
func TestEnumSchemaDeclaresTheArguments(t *testing.T) {
	tl := &EnumTool{}
	schema := tl.Schema()
	properties, _ := schema["properties"].(map[string]any)
	for _, name := range []string{"mode", "domain", "name", "host", "types"} {
		if _, ok := properties[name]; !ok {
			t.Errorf("the schema does not declare %q", name)
		}
	}
	mode, _ := properties["mode"].(map[string]any)
	modes, _ := mode["enum"].([]string)
	if strings.Join(modes, ",") != "subdomains,dns,host" {
		t.Errorf("the mode enum is %v, want subdomains, dns, host", modes)
	}
	if err := ValidateArguments(schema, map[string]any{
		"mode":  "dns",
		"name":  "example.com",
		"types": []any{"A", "TXT"},
	}); err != nil {
		t.Errorf("the harness refuses the declared arguments: %v", err)
	}
	if err := ValidateArguments(schema, map[string]any{"mode": "nonsense"}); err == nil {
		t.Error("the harness accepted a mode that is not one of the three")
	}
}

// TestEnumTranslateNeedsTheArgumentForItsMode: the mode decides which
// argument is required.
func TestEnumTranslateNeedsTheArgumentForItsMode(t *testing.T) {
	tl := &EnumTool{}
	cases := []map[string]any{
		{"mode": "subdomains"},
		{"mode": "subdomains", "domain": "  "},
		{"mode": "dns"},
		{"mode": "host"},
		{"mode": "nonsense", "domain": "example.com"},
	}
	for _, args := range cases {
		if spec, err := tl.Translate(args); err == nil {
			t.Errorf("Translate(%v) accepted, spec = %v", args, spec)
		}
	}
}

// TestEnumTranslateBuildsTheSpec: the operation type is the registered tool
// name, and the canonical argument map carries what the caller asked for.
func TestEnumTranslateBuildsTheSpec(t *testing.T) {
	tl := &EnumTool{}

	spec, err := tl.Translate(map[string]any{"mode": "dns", "name": "example.com", "types": []any{"A", "TXT"}})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if spec.Type != "enum" {
		t.Errorf("spec.Type = %q, want enum", spec.Type)
	}
	if spec.Args["name"] != "example.com" {
		t.Errorf("spec.Args[name] = %v", spec.Args["name"])
	}
	types, _ := spec.Args["types"].([]string)
	if len(types) != 2 || types[0] != "A" {
		t.Errorf("spec.Args[types] = %v", spec.Args["types"])
	}

	// The mode defaults to subdomains, and an absent list is not put into
	// the spec.
	spec, err = tl.Translate(map[string]any{"domain": "example.com"})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if spec.Args["mode"] != "subdomains" {
		t.Errorf("spec.Args[mode] = %v, want subdomains", spec.Args["mode"])
	}
	if _, ok := spec.Args["types"]; ok {
		t.Errorf("an absent list was put in the spec: %v", spec.Args)
	}
}

// TestEnumSubdomainsMode: the names, one per line, then the count and the
// source that answered.
func TestEnumSubdomainsMode(t *testing.T) {
	transport := &enumFakeTransport{
		keys: []string{"crt.sh"},
		answers: map[string]string{
			"crt.sh": `[{"common_name":"www.example.com","name_value":"www.example.com\nmail.example.com"}]`,
		},
	}
	tl := enumTool(t, transport, &enumFakeResolver{})

	spec, err := tl.Translate(map[string]any{"mode": "subdomains", "domain": "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunContext: %v", err)
	}
	for _, want := range []string{"www.example.com", "mail.example.com", "2 names, source: crt.sh"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output does not carry %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "name_value") {
		t.Errorf("the output carries the raw body:\n%s", out)
	}
}

// TestEnumDNSMode: one record per line as "TYPE value", then the count.
func TestEnumDNSMode(t *testing.T) {
	resolver := &enumFakeResolver{
		addresses: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}},
		txt:       []string{"v=spf1 -all"},
	}
	tl := enumTool(t, &enumFakeTransport{}, resolver)

	spec, err := tl.Translate(map[string]any{"mode": "dns", "name": "example.com", "types": []any{"A", "TXT"}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunContext: %v", err)
	}
	for _, want := range []string{"A 93.184.216.34", "TXT v=spf1 -all", "2 records"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output does not carry %q:\n%s", want, out)
		}
	}
}

// TestEnumHostMode: the fields that are known, one per line, naming the
// source of the prefix and of the AS number.
func TestEnumHostMode(t *testing.T) {
	transport := &enumFakeTransport{
		keys: []string{"rdap.org", "stat.ripe.net"},
		answers: map[string]string{
			"rdap.org": `{"startAddress":"8.8.8.0","endAddress":"8.8.8.255","name":"GOOGLE","country":"US",
				"entities":[{"handle":"GOGL","roles":["registrant"],"vcardArray":["vcard",[["fn",{},"text","Google LLC"]]]}]}`,
			"stat.ripe.net": `{"data":{"prefix":"8.8.8.0/24","asns":["AS15169"],"holder":"GOOGLE"}}`,
		},
	}
	tl := enumTool(t, transport, &enumFakeResolver{})

	spec, err := tl.Translate(map[string]any{"mode": "host", "host": "8.8.8.8"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunContext: %v", err)
	}
	for _, want := range []string{
		"ip 8.8.8.8",
		"prefix 8.8.8.0/24 (source: rdap.org)",
		"asn AS15169 (source: stat.ripe.net)",
		"as name GOOGLE",
		"country US",
		"registrant Google LLC",
		"source rdap.org+stat.ripe.net",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output does not carry %q:\n%s", want, out)
		}
	}
}

// TestEnumRunRefusesAnEmptyCall: the mode still decides what is required at
// run time, because a spec can arrive from a transcript as well as from a
// translation.
func TestEnumRunRefusesAnEmptyCall(t *testing.T) {
	tl := enumTool(t, &enumFakeTransport{}, &enumFakeResolver{})
	if _, err := tl.RunContext(context.Background(), operation.Spec{Type: "enum", Args: map[string]any{"mode": "dns"}}); err == nil {
		t.Error("a dns call without a name was accepted")
	}
	if _, err := tl.RunContext(context.Background(), operation.Spec{Type: "enum", Args: map[string]any{"mode": "nonsense"}}); err == nil {
		t.Error("a call with an unknown mode was accepted")
	}
}

// TestEnumTranslateResultPassesOutput: result translation is a pass through
// of the recorded output.
func TestEnumTranslateResultPassesOutput(t *testing.T) {
	tl := &EnumTool{}
	op := operation.Operation{Output: "3 names, source: crt.sh"}
	if got := tl.TranslateResult(op); got != op.Output {
		t.Errorf("TranslateResult = %q, want passthrough", got)
	}
}
