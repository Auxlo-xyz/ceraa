package enum

// rdap.go is the network registration half of passive reconnaissance: what
// the registries say about an address, and therefore about the network it
// sits in.
//
// rdap.org is a redirector: it answers with a 302 to the authoritative RDAP
// server for the address, and that server holds the record. The hop is
// followed because skipping it means never asking the authority at all, and
// it is gated and charged like any other request.
//
// The registration records the allocation. The AS number lives with the
// routing registry instead, so stat.ripe.net fills the gap when rdap.org
// does not answer one.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
)

const (
	// sourceRDAP is the registration source. It redirects to whichever
	// registry holds the address.
	sourceRDAP = "rdap.org"
	// sourceRIPE is the routing registry's statistics service. It answers
	// with the announced prefix and the AS that announces it.
	sourceRIPE = "stat.ripe.net"
)

// HostInfo is what the network tells about an address.
type HostInfo struct {
	IP         string
	ASN        string
	ASName     string
	Country    string
	Prefix     string // the announced CIDR, e.g. 8.8.8.0/24
	Registrant string // the org that holds the allocation
	Source     string // which source answered
	// PrefixSource names the source the prefix came from. It can differ
	// from Source: the registration records the allocation and the routing
	// registry records the announced prefix, and one of the two can answer
	// what the other left empty.
	PrefixSource string
	// ASNSource names the source the AS number came from, for the same
	// reason.
	ASNSource string
}

// LookupHost looks up the network registration for an IP or a hostname.
//
// A hostname is resolved first and the first address is used: a name with two
// address families is one host, and the registration of either address is the
// registration of the host.
func LookupHost(ctx context.Context, host string) (HostInfo, error) {
	return CollectHost(ctx, Options{}, host)
}

// CollectHost is LookupHost with the injectable seams.
func CollectHost(ctx context.Context, opts Options, host string) (HostInfo, error) {
	target := strings.TrimSpace(host)
	if target == "" {
		return HostInfo{}, fmt.Errorf("host is required")
	}
	if strings.ContainsAny(target, " \t\r\n") {
		return HostInfo{}, fmt.Errorf("host %q contains whitespace", host)
	}
	ctx, cancel := context.WithTimeout(ctx, opts.timeout())
	defer cancel()

	ip, err := resolveIP(ctx, opts, target)
	if err != nil {
		return HostInfo{}, err
	}

	info := HostInfo{IP: ip}
	var answered []string

	registration, regErr := lookupRDAP(ctx, opts, ip)
	if regErr == nil {
		info = registration
		info.IP = ip
		answered = append(answered, sourceRDAP)
	}

	// The routing registry fills what the registration left empty. An AS
	// number is the usual gap: the registration says who holds the
	// allocation, the routing registry says which autonomous system
	// announces it.
	if regErr != nil || info.Prefix == "" || info.ASN == "" {
		routing, routeErr := lookupRIPE(ctx, opts, ip)
		if routeErr == nil {
			if info.Prefix == "" {
				info.Prefix = routing.Prefix
				info.PrefixSource = sourceRIPE
			}
			if info.ASN == "" {
				info.ASN = routing.ASN
				info.ASNSource = sourceRIPE
			}
			if info.ASName == "" {
				info.ASName = routing.ASName
			}
			answered = append(answered, sourceRIPE)
		} else if regErr != nil {
			return HostInfo{}, fmt.Errorf("network registration for %s: %s: %v; %s: %v", ip, sourceRDAP, regErr, sourceRIPE, routeErr)
		}
	}
	info.Source = strings.Join(answered, "+")
	if info.Source == "" {
		return HostInfo{}, fmt.Errorf("network registration for %s: no source answered", ip)
	}
	return info, nil
}

// resolveIP turns a hostname into an address.
func resolveIP(ctx context.Context, opts Options, host string) (string, error) {
	trimmed := strings.Trim(strings.TrimSpace(host), "[]")
	if ip := net.ParseIP(trimmed); ip != nil {
		return ip.String(), nil
	}
	addrs, err := opts.resolver().LookupIPAddr(ctx, host)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, addr := range addrs {
		if addr.IP != nil {
			return addr.IP.String(), nil
		}
	}
	return "", fmt.Errorf("%s did not resolve to an address", host)
}

// lookupRDAP asks rdap.org for the registration of one address.
func lookupRDAP(ctx context.Context, opts Options, ip string) (HostInfo, error) {
	_, body, err := fetch(ctx, opts, "https://"+sourceRDAP+"/ip/"+ip, sourceRDAP, maxRedirects)
	if err != nil {
		return HostInfo{}, err
	}
	var doc rdapIP
	if err := json.Unmarshal(body, &doc); err != nil {
		return HostInfo{}, fmt.Errorf("%s: the answer was not the expected JSON: %w", sourceRDAP, err)
	}
	info := HostInfo{
		IP:         ip,
		ASN:        doc.asn(),
		ASName:     strings.TrimSpace(doc.Name),
		Country:    doc.Country.value(),
		Prefix:     doc.prefix(),
		Registrant: doc.registrant(),
	}
	if info.Prefix != "" {
		info.PrefixSource = sourceRDAP
	}
	if info.ASN != "" {
		info.ASNSource = sourceRDAP
	}
	return info, nil
}

// lookupRIPE asks the routing registry's statistics service about one
// address.
func lookupRIPE(ctx context.Context, opts Options, ip string) (HostInfo, error) {
	endpoint := "https://" + sourceRIPE + "/data/prefix-overview/data.json?resource=" + ip
	_, body, err := fetch(ctx, opts, endpoint, sourceRIPE, 0)
	if err != nil {
		return HostInfo{}, err
	}
	var doc ripeOverview
	if err := json.Unmarshal(body, &doc); err != nil {
		return HostInfo{}, fmt.Errorf("%s: the answer was not the expected JSON: %w", sourceRIPE, err)
	}
	info := HostInfo{
		IP:     ip,
		ASN:    doc.Data.asn(),
		ASName: strings.TrimSpace(doc.Data.Holder),
		Prefix: doc.Data.Prefix,
	}
	if info.Prefix == "" {
		info.Prefix = strings.TrimSpace(doc.Data.Resource)
	}
	return info, nil
}

// rdapIP is the part of an RDAP IP response that matters here.
type rdapIP struct {
	StartAddress string          `json:"startAddress"`
	EndAddress   string          `json:"endAddress"`
	Name         string          `json:"name"`
	Type         string          `json:"type"`
	Country      countryValue    `json:"country"`
	Handle       string          `json:"handle"`
	ASN          json.RawMessage `json:"asn"`
	CIDR0CIDRs   []struct {
		V4Prefix string `json:"v4prefix"`
		V6Prefix string `json:"v6prefix"`
		Length   int    `json:"length"`
	} `json:"cidr0_cidrs"`
	Entities []rdapEntity `json:"entities"`
}

// prefix reads the announced block. The cidr0_cidrs list is authoritative
// when it is there; the start and end addresses are the fallback, and the
// block they describe is the smallest CIDR that holds both.
func (d rdapIP) prefix() string {
	for _, entry := range d.CIDR0CIDRs {
		if entry.V4Prefix != "" {
			return fmt.Sprintf("%s/%d", entry.V4Prefix, entry.Length)
		}
		if entry.V6Prefix != "" {
			return fmt.Sprintf("%s/%d", entry.V6Prefix, entry.Length)
		}
	}
	return prefixFromRange(d.StartAddress, d.EndAddress)
}

// asn reads the AS number when the registry carries one. Most do not, and
// the field is left empty rather than filled from the organisation name.
func (d rdapIP) asn() string {
	return asnText(d.ASN)
}

// registrant reads the entity that holds the allocation. The vcard carries a
// readable name for it and the handle is the fallback, because a handle is
// still an answer when the name is missing.
func (d rdapIP) registrant() string {
	for _, entity := range d.Entities {
		if !entity.hasRole("registrant") {
			continue
		}
		if name := entity.fn(); name != "" {
			return name
		}
		if entity.Handle != "" {
			return entity.Handle
		}
	}
	for _, entity := range d.Entities {
		if name := entity.fn(); name != "" {
			return name
		}
	}
	if d.Handle != "" {
		return d.Handle
	}
	return ""
}

// rdapEntity is one organisation named in an RDAP response.
type rdapEntity struct {
	Handle     string          `json:"handle"`
	Roles      []string        `json:"roles"`
	VCardArray json.RawMessage `json:"vcardArray"`
}

// hasRole reports whether the entity carries a role.
func (e rdapEntity) hasRole(role string) bool {
	for _, held := range e.Roles {
		if strings.EqualFold(strings.TrimSpace(held), role) {
			return true
		}
	}
	return false
}

// fn reads the readable name out of the vcard.
func (e rdapEntity) fn() string {
	return vcardField(e.VCardArray, "fn")
}

// vcardField reads one field out of a jCard. The shape is a list whose
// second element holds the fields, each a list of name, parameters, type and
// value.
func vcardField(raw json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var card []json.RawMessage
	if err := json.Unmarshal(raw, &card); err != nil || len(card) < 2 {
		return ""
	}
	var fields [][]json.RawMessage
	if err := json.Unmarshal(card[1], &fields); err != nil {
		return ""
	}
	for _, field := range fields {
		if len(field) < 4 {
			continue
		}
		var name string
		if err := json.Unmarshal(field[0], &name); err != nil {
			continue
		}
		if !strings.EqualFold(name, key) {
			continue
		}
		var value string
		if err := json.Unmarshal(field[3], &value); err == nil {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// countryValue reads the country field, which registries write either as a
// plain code or as an object carrying the country name.
type countryValue struct {
	Name string
	Code string
}

// UnmarshalJSON reads both spellings.
func (c *countryValue) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if strings.HasPrefix(trimmed, "{") {
		var object struct {
			Name string `json:"name"`
			ISO2 string `json:"iso2"`
			ISO3 string `json:"iso3"`
		}
		if err := json.Unmarshal(data, &object); err != nil {
			return err
		}
		c.Name = strings.TrimSpace(object.Name)
		c.Code = strings.TrimSpace(object.ISO2)
		if c.Code == "" {
			c.Code = strings.TrimSpace(object.ISO3)
		}
		return nil
	}
	var code string
	if err := json.Unmarshal(data, &code); err != nil {
		return err
	}
	c.Code = strings.TrimSpace(code)
	return nil
}

// value is the country name when the registry gave one and the code
// otherwise.
func (c countryValue) value() string {
	if c.Name != "" {
		return c.Name
	}
	return c.Code
}

// ripeOverview is the routing registry's answer for one address.
type ripeOverview struct {
	Status string   `json:"status"`
	Data   ripeData `json:"data"`
}

// ripeData is the part of the answer that names the block and the AS.
type ripeData struct {
	Resource string   `json:"resource"`
	Prefix   string   `json:"prefix"`
	Holder   string   `json:"holder"`
	ASN      int      `json:"asn"`
	ASNs     []string `json:"asns"`
}

// asn reads the AS number, which the service writes as a number and as a
// list of names.
func (d ripeData) asn() string {
	for _, entry := range d.ASNs {
		entry = strings.TrimSpace(entry)
		if entry != "" {
			return entry
		}
	}
	if d.ASN > 0 {
		return fmt.Sprintf("AS%d", d.ASN)
	}
	return ""
}

// prefixFromRange turns the start and end of an allocation into the CIDR
// that covers it.
//
// The registry states a range, not a prefix, and the range is the smallest
// block that contains both ends. That is the allocation, and it is what a
// routing table would announce.
func prefixFromRange(start string, end string) string {
	first := net.ParseIP(strings.TrimSpace(start))
	last := net.ParseIP(strings.TrimSpace(end))
	if first == nil || last == nil {
		return ""
	}
	firstV4, lastV4 := first.To4(), last.To4()
	if (firstV4 != nil) != (lastV4 != nil) {
		return ""
	}
	bits := 128
	low, high := first.To16(), last.To16()
	if firstV4 != nil {
		bits = 32
		low, high = firstV4, lastV4
	}
	length := commonPrefixLength(low, high)
	mask := net.CIDRMask(length, bits)
	if mask == nil {
		return ""
	}
	return (&net.IPNet{IP: low.Mask(mask), Mask: mask}).String()
}

// commonPrefixLength counts the leading bits two addresses share.
func commonPrefixLength(a net.IP, b net.IP) int {
	if a == nil || b == nil || len(a) != len(b) {
		return 0
	}
	total := 0
	for i := range a {
		if a[i] == b[i] {
			total += 8
			continue
		}
		difference := int(a[i]) ^ int(b[i])
		for bit := 7; bit >= 0; bit-- {
			if difference&(1<<bit) != 0 {
				break
			}
			total++
		}
		break
	}
	return total
}

// asnText reads an AS number written either as a number or as a name.
func asnText(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		if value, err := number.Int64(); err == nil && value > 0 {
			return fmt.Sprintf("AS%d", value)
		}
	}
	return ""
}
