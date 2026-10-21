package enum

// dns.go is the resolver half of passive reconnaissance.
//
// Two rules shape it. A CNAME is reported, not chased: the chase is a set of
// requests the caller did not ask for, and the target is named so the caller
// can ask for it. And a TXT value is bounded: a TXT record is a place people
// put things that are kilobytes long, and an unbounded one is a wall of text
// in a chat message.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
)

// maxTXTRunes bounds one TXT value. Long enough for a SPF policy or a
// verification token, short enough that a misconfigured record cannot fill a
// chat message.
const maxTXTRunes = 1000

// defaultDNSTypes is what a caller gets when it does not say what it wants.
// SOA is absent on purpose: the Go resolver does not expose it, and a record
// type that cannot be looked up is not worth offering by default.
var defaultDNSTypes = []string{"A", "AAAA", "CNAME", "MX", "NS", "TXT"}

// supportedDNSTypes are the types the resolver seam can answer.
var supportedDNSTypes = map[string]bool{
	"A":     true,
	"AAAA":  true,
	"CNAME": true,
	"MX":    true,
	"NS":    true,
	"TXT":   true,
}

// DNSRecord is one answer from a resolver.
type DNSRecord struct {
	Name  string
	Type  string // A, AAAA, MX, NS, TXT, CNAME, SOA
	Value string
}

// DNSResult is the answer plus the notes that came with it.
type DNSResult struct {
	// Records are the answers, sorted by type and then by name.
	Records []DNSRecord
	// Notes carry the honest remarks: a type that could not be looked up,
	// a value that was cut, a CNAME that was not chased.
	Notes []string
}

// DNSRecords resolves a name for the given record types.
//
// The types default to A, AAAA, CNAME, MX, NS and TXT. A type that cannot be
// looked up does not sink the others: the answers that came back are worth
// having, and the note says which one did not.
func DNSRecords(ctx context.Context, name string, types []string) ([]DNSRecord, error) {
	result, err := CollectDNS(ctx, Options{}, name, types)
	if err != nil {
		return nil, err
	}
	return result.Records, nil
}

// CollectDNS is DNSRecords with the notes and the injectable seams.
func CollectDNS(ctx context.Context, opts Options, name string, types []string) (DNSResult, error) {
	host := strings.TrimSpace(name)
	if host == "" {
		return DNSResult{}, fmt.Errorf("name is required")
	}
	if strings.ContainsAny(host, " \t\r\n") {
		return DNSResult{}, fmt.Errorf("name %q contains whitespace", name)
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")

	ctx, cancel := context.WithTimeout(ctx, opts.timeout())
	defer cancel()

	wanted, unsupported := wantedTypes(types)
	result := DNSResult{Notes: unsupported}

	var failures []string
	for _, kind := range wanted {
		records, notes, err := lookupType(ctx, opts.resolver(), host, kind)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", kind, err))
			continue
		}
		result.Records = append(result.Records, records...)
		result.Notes = append(result.Notes, notes...)
	}
	if len(result.Records) == 0 && len(failures) > 0 {
		return DNSResult{}, fmt.Errorf("dns lookup of %s failed: %s", host, strings.Join(failures, "; "))
	}
	result.Notes = append(result.Notes, failures...)

	sort.Slice(result.Records, func(i, j int) bool {
		left, right := result.Records[i], result.Records[j]
		if left.Type != right.Type {
			return left.Type < right.Type
		}
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		return left.Value < right.Value
	})
	return result, nil
}

// wantedTypes splits the caller's types into the ones that can be looked up
// and the ones that cannot, with a note for each of the latter.
func wantedTypes(types []string) ([]string, []string) {
	list := types
	if len(list) == 0 {
		list = defaultDNSTypes
	}
	var wanted []string
	var notes []string
	seen := make(map[string]bool)
	for _, raw := range list {
		kind := strings.ToUpper(strings.TrimSpace(raw))
		if kind == "" {
			continue
		}
		if seen[kind] {
			continue
		}
		seen[kind] = true
		if kind == "SOA" {
			// The Go resolver exposes no SOA lookup, and a record type
			// that cannot be asked for is said so rather than answered
			// with a guess.
			notes = append(notes, "SOA cannot be looked up through the system resolver")
			continue
		}
		if !supportedDNSTypes[kind] {
			notes = append(notes, fmt.Sprintf("%s is not a record type this lookup answers", kind))
			continue
		}
		wanted = append(wanted, kind)
	}
	return wanted, notes
}

// lookupType asks the resolver for one type and returns the answers, the
// notes and the failure.
func lookupType(ctx context.Context, resolver Resolver, host string, kind string) ([]DNSRecord, []string, error) {
	switch kind {
	case "A", "AAAA":
		addrs, err := resolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, nil, err
		}
		var out []DNSRecord
		for _, addr := range addrs {
			if addr.IP == nil {
				continue
			}
			isV4 := addr.IP.To4() != nil
			if (kind == "A") != isV4 {
				continue
			}
			out = append(out, DNSRecord{Name: host, Type: kind, Value: addr.IP.String()})
		}
		return out, nil, nil

	case "CNAME":
		target, err := resolver.LookupCNAME(ctx, host)
		if err != nil {
			if isNotFound(err) {
				// A name without a CNAME is an answer, not a failure.
				return nil, nil, nil
			}
			return nil, nil, err
		}
		target = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(target)), ".")
		if target == "" {
			return nil, nil, nil
		}
		notes := []string{fmt.Sprintf("the CNAME chain was not chased; %s is the next name to look up", target)}
		return []DNSRecord{{Name: host, Type: "CNAME", Value: target}}, notes, nil

	case "MX":
		mxs, err := resolver.LookupMX(ctx, host)
		if err != nil {
			return nil, nil, err
		}
		var out []DNSRecord
		for _, mx := range mxs {
			if mx == nil {
				continue
			}
			target := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(mx.Host)), ".")
			out = append(out, DNSRecord{Name: host, Type: "MX", Value: fmt.Sprintf("%d %s", mx.Pref, target)})
		}
		return out, nil, nil

	case "NS":
		nss, err := resolver.LookupNS(ctx, host)
		if err != nil {
			return nil, nil, err
		}
		var out []DNSRecord
		for _, ns := range nss {
			if ns == nil {
				continue
			}
			target := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(ns.Host)), ".")
			out = append(out, DNSRecord{Name: host, Type: "NS", Value: target})
		}
		return out, nil, nil

	case "TXT":
		txts, err := resolver.LookupTXT(ctx, host)
		if err != nil {
			return nil, nil, err
		}
		var out []DNSRecord
		var notes []string
		for _, txt := range txts {
			value := txt
			runes := []rune(value)
			if len(runes) > maxTXTRunes {
				value = string(runes[:maxTXTRunes])
				notes = append(notes, fmt.Sprintf("a TXT value for %s was cut at %d runes", host, maxTXTRunes))
			}
			out = append(out, DNSRecord{Name: host, Type: "TXT", Value: value})
		}
		return out, notes, nil
	}
	return nil, nil, fmt.Errorf("%s is not a record type this lookup answers", kind)
}

// isNotFound reports the resolver's "no such record". It is an answer rather
// than a failure, and treating it as a failure would report a name without a
// CNAME as a broken lookup.
func isNotFound(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsNotFound
	}
	return false
}
