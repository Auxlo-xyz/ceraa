package tool

import (
	"context"
	"fmt"
	"strings"

	"ceraa/harness/enum"
	"ceraa/harness/operation"
)

// EnumTool is the model-facing face of passive reconnaissance: the names a
// domain is known by, the DNS behind a name, and the network a host sits in.
//
// It only shapes arguments and renders answers. The sources, the fallbacks
// and the egress wall live in harness/enum, so the tool and the engine cannot
// drift apart.
type EnumTool struct {
	// Options is the enum seam. The zero value talks to the real world
	// through the egress wall; a test fills it with a fake doer and never
	// leaves the machine.
	Options enum.Options
}

func (e *EnumTool) Name() string { return "enum" }

func (e *EnumTool) Description() string {
	return "Passive reconnaissance from public sources: nothing is probed and nothing is submitted. mode subdomains reads the certificate transparency logs (crt.sh, falling back to Cert Spotter) for the names a domain is known by. mode dns resolves a name through the system resolver (A, AAAA, CNAME, MX, NS and TXT unless the types argument says otherwise). mode host looks up the network registration of an IP or a hostname (rdap.org, falling back to stat.ripe.net): the announced prefix, the AS number and its organisation, the country and the registrant. Every outbound request is scope-gated and charged to the engagement budget. Returns one bounded line per name, record or field, names the source that answered, and leaves an unknown field empty instead of a guess."
}

func (e *EnumTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode": map[string]any{
				"type":        "string",
				"enum":        []string{"subdomains", "dns", "host"},
				"default":     "subdomains",
				"description": "What to collect: subdomains from certificate transparency, dns records for a name, or the network registration of a host",
			},
			"domain": map[string]any{"type": "string", "description": "The domain to enumerate subdomains for (mode subdomains). A scheme, a path and a trailing dot are ignored"},
			"name":   map[string]any{"type": "string", "description": "The name to resolve (mode dns)"},
			"host":   map[string]any{"type": "string", "description": "The IP address or hostname to look up (mode host)"},
			"types": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "The DNS record types to ask for (mode dns): A, AAAA, CNAME, MX, NS, TXT. Defaults to all of them",
			},
		},
	}
}

// Translate validates the shape and re-emits the canonical argument map.
//
// The mode decides which argument is required, so the requirement cannot live
// in the schema: a schema that always wanted a domain would refuse a DNS
// lookup, and one that wanted nothing would let an empty call through.
func (e *EnumTool) Translate(args map[string]any) (operation.Spec, error) {
	mode := strings.TrimSpace(enumStringArg(args["mode"]))
	if mode == "" {
		mode = "subdomains"
	}
	out := map[string]any{"mode": mode}
	switch mode {
	case "subdomains":
		domain := strings.TrimSpace(enumStringArg(args["domain"]))
		if domain == "" {
			return operation.Spec{}, fmt.Errorf("mode subdomains needs a domain")
		}
		out["domain"] = domain
	case "dns":
		name := strings.TrimSpace(enumStringArg(args["name"]))
		if name == "" {
			return operation.Spec{}, fmt.Errorf("mode dns needs a name")
		}
		out["name"] = name
		if types := enumStringListArg(args["types"]); len(types) > 0 {
			out["types"] = types
		}
	case "host":
		host := strings.TrimSpace(enumStringArg(args["host"]))
		if host == "" {
			return operation.Spec{}, fmt.Errorf("mode host needs a host")
		}
		out["host"] = host
	default:
		return operation.Spec{}, fmt.Errorf("mode %q is not one of subdomains, dns, host", mode)
	}
	return operation.Spec{Type: "enum", Args: out}, nil
}

func (e *EnumTool) TranslateResult(op operation.Operation) string { return op.Output }

func (e *EnumTool) Run(spec operation.Spec) (string, error) {
	return e.RunContext(context.Background(), spec)
}

// RunContext dispatches on the mode and renders the answer.
//
// The output is bounded by construction: the name list is capped by the
// engine, the record list is a handful of lines and the host answer is a
// handful of fields. No HTTP body is ever rendered.
func (e *EnumTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	mode := strings.TrimSpace(enumStringArg(spec.Args["mode"]))
	if mode == "" {
		mode = "subdomains"
	}
	switch mode {
	case "subdomains":
		domain := strings.TrimSpace(enumStringArg(spec.Args["domain"]))
		if domain == "" {
			return "", fmt.Errorf("mode subdomains needs a domain")
		}
		result, err := enum.CollectSubdomains(ctx, e.Options, domain)
		if err != nil {
			return "", err
		}
		return formatEnumSubdomains(result), nil
	case "dns":
		name := strings.TrimSpace(enumStringArg(spec.Args["name"]))
		if name == "" {
			return "", fmt.Errorf("mode dns needs a name")
		}
		result, err := enum.CollectDNS(ctx, e.Options, name, enumStringListArg(spec.Args["types"]))
		if err != nil {
			return "", err
		}
		return formatEnumDNS(result), nil
	case "host":
		host := strings.TrimSpace(enumStringArg(spec.Args["host"]))
		if host == "" {
			return "", fmt.Errorf("mode host needs a host")
		}
		info, err := enum.CollectHost(ctx, e.Options, host)
		if err != nil {
			return "", err
		}
		return formatEnumHost(info), nil
	}
	return "", fmt.Errorf("mode %q is not one of subdomains, dns, host", mode)
}

// formatEnumSubdomains renders one name per line, then the count and the
// source that answered.
func formatEnumSubdomains(result enum.SubdomainResult) string {
	var b strings.Builder
	for _, name := range result.Names {
		b.WriteString(name)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "%d names, source: %s", len(result.Names), result.Source)
	if result.Truncated {
		b.WriteString(" (truncated)")
	}
	for _, note := range result.Notes {
		fmt.Fprintf(&b, "\nnote: %s", note)
	}
	return b.String()
}

// formatEnumDNS renders one record per line as "TYPE value", then the count.
func formatEnumDNS(result enum.DNSResult) string {
	var b strings.Builder
	for _, record := range result.Records {
		fmt.Fprintf(&b, "%s %s\n", record.Type, record.Value)
	}
	fmt.Fprintf(&b, "%d records", len(result.Records))
	for _, note := range result.Notes {
		fmt.Fprintf(&b, "\nnote: %s", note)
	}
	return b.String()
}

// formatEnumHost renders the fields that are known, one per line, naming the
// source of the prefix and of the AS number.
func formatEnumHost(info enum.HostInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ip %s\n", info.IP)
	if info.Prefix != "" {
		fmt.Fprintf(&b, "prefix %s (source: %s)\n", info.Prefix, enumSourceOr(info.PrefixSource, info.Source))
	}
	if info.ASN != "" {
		fmt.Fprintf(&b, "asn %s (source: %s)\n", info.ASN, enumSourceOr(info.ASNSource, info.Source))
	}
	if info.ASName != "" {
		fmt.Fprintf(&b, "as name %s\n", info.ASName)
	}
	if info.Country != "" {
		fmt.Fprintf(&b, "country %s\n", info.Country)
	}
	if info.Registrant != "" {
		fmt.Fprintf(&b, "registrant %s\n", info.Registrant)
	}
	fmt.Fprintf(&b, "source %s", info.Source)
	return b.String()
}

// enumSourceOr falls back to the overall source when a field carries none.
func enumSourceOr(field string, overall string) string {
	if field != "" {
		return field
	}
	return overall
}

// enumStringArg reads a string argument without the zero-value trap.
func enumStringArg(v any) string {
	s, _ := v.(string)
	return s
}

// enumStringListArg reads a list of strings in both the native and the
// JSON-decoded shape, dropping the entries that are not strings.
func enumStringListArg(v any) []string {
	switch typed := v.(type) {
	case []string:
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, entry := range typed {
			if s, ok := entry.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	}
	return nil
}
