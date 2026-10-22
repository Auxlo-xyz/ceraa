package tool

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"ceraa/harness/act"
	"ceraa/harness/operation"
)

// The bounds of what the tool renders.
//
// A directory with a hundred thousand users must not be able to fill a
// chat message, and an entry with two hundred attributes must not either.
const (
	// ldapDefaultLimit is the number of entries one enumeration asks for.
	ldapDefaultLimit = 100
	// ldapMaxLimit is the ceiling on the entries one enumeration may ask
	// for.
	ldapMaxLimit = 1000
	// ldapMaxRenderedEntries is the number of entries the output lists.
	ldapMaxRenderedEntries = 50
	// ldapMaxRenderedAttributes is the number of attributes listed for one
	// entry.
	ldapMaxRenderedAttributes = 12
	// ldapMaxRenderedValues is the number of values listed for one
	// attribute.
	ldapMaxRenderedValues = 4
	// ldapMaxValueRunes bounds one rendered value.
	ldapMaxValueRunes = 160
	// ldapDefaultTimeout is the operation timeout in seconds.
	ldapDefaultTimeout = 15
	// ldapMaxTimeout is the ceiling on the timeout in seconds.
	ldapMaxTimeout = 120
)

// LDAPTool is the model-facing face of the directory: a bind, a search, and
// the enumerations that decide whether a domain is worth the next hour.
//
// It only shapes arguments and renders answers. The protocol lives in
// harness/act, so the tool and the engine cannot drift apart.
type LDAPTool struct{}

func (l *LDAPTool) Name() string { return "ldap" }

func (l *LDAPTool) Description() string {
	return "Read an Active Directory over LDAP or LDAPS: the rootDSE, the users, groups, computers, service principal names and trusts. Credential attributes are dropped, so a hash never reaches the transcript."
}

func (l *LDAPTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"host": map[string]any{"type": "string", "description": "The domain controller"},
			"port": map[string]any{"type": "integer", "minimum": 1, "maximum": 65535, "description": "The port, default 389"},
			"scheme": map[string]any{
				"type":        "string",
				"enum":        []string{"ldap", "ldaps"},
				"default":     "ldap",
				"description": "ldap or ldaps",
			},
			"bind_dn":       map[string]any{"type": "string", "description": "The DN to bind as, empty binds anonymously"},
			"bind_password": map[string]any{"type": "string", "description": "The bind password"},
			"domain":        map[string]any{"type": "string", "description": "The domain to enumerate, as example.com"},
			"mode": map[string]any{
				"type":        "string",
				"enum":        []string{"rootdse", "users", "groups", "computers", "spn", "trusts"},
				"default":     "rootdse",
				"description": "rootdse, users, groups, computers, spn or trusts",
			},
			"base":   map[string]any{"type": "string", "description": "The base DN, as DC=example,DC=com"},
			"filter": map[string]any{"type": "string", "description": "An extra LDAP filter"},
			"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 1000, "description": "Maximum entries, default 100"},
			"timeout": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"maximum":     120,
				"description": "Timeout in seconds, default 15",
			},
		},
		"required": []string{"host"},
	}
}

// Translate validates the shape and re-emits the canonical argument map.
//
// The password is carried in the arguments because the bind needs it, and
// it is carried nowhere else: the tool never renders it and the engine
// never logs it.
func (l *LDAPTool) Translate(args map[string]any) (operation.Spec, error) {
	host := strings.TrimSpace(ldapStringArg(args["host"]))
	if host == "" {
		return operation.Spec{}, fmt.Errorf("host is required")
	}
	if strings.ContainsAny(host, " \t\r\n") {
		return operation.Spec{}, fmt.Errorf("host %q is not a hostname", host)
	}
	scheme := strings.ToLower(strings.TrimSpace(ldapStringArg(args["scheme"])))
	if scheme == "" {
		scheme = "ldap"
	}
	if scheme != "ldap" && scheme != "ldaps" {
		return operation.Spec{}, fmt.Errorf("scheme %q is not one of ldap, ldaps", scheme)
	}
	mode := strings.ToLower(strings.TrimSpace(ldapStringArg(args["mode"])))
	if mode == "" {
		mode = "rootdse"
	}
	switch mode {
	case "rootdse", "users", "groups", "computers", "spn", "trusts":
	default:
		return operation.Spec{}, fmt.Errorf("mode %q is not one of rootdse, users, groups, computers, spn, trusts", mode)
	}

	port := ldapDefaultPort(scheme)
	if value, ok := ldapIntArg(args["port"]); ok && value > 0 {
		if value > 65535 {
			return operation.Spec{}, fmt.Errorf("port %d is out of range", value)
		}
		port = value
	}
	limit := ldapDefaultLimit
	if value, ok := ldapIntArg(args["limit"]); ok && value > 0 {
		limit = value
	}
	if limit > ldapMaxLimit {
		limit = ldapMaxLimit
	}
	timeout := ldapDefaultTimeout
	if value, ok := ldapIntArg(args["timeout"]); ok && value > 0 {
		timeout = value
	}
	if timeout > ldapMaxTimeout {
		timeout = ldapMaxTimeout
	}

	out := map[string]any{
		"host":    host,
		"port":    port,
		"scheme":  scheme,
		"mode":    mode,
		"limit":   limit,
		"timeout": timeout,
	}
	domain := strings.TrimSpace(ldapStringArg(args["domain"]))
	base := strings.TrimSpace(ldapStringArg(args["base"]))
	if mode != "rootdse" && domain == "" && base == "" {
		// The enumeration needs somewhere to search, and the call is one
		// shot: a refusal here is a refusal the model can correct.
		return operation.Spec{}, fmt.Errorf("mode %s needs a domain or a base to search under", mode)
	}
	if value := ldapStringArg(args["bind_dn"]); strings.TrimSpace(value) != "" {
		out["bind_dn"] = strings.TrimSpace(value)
	}
	if value := ldapStringArg(args["bind_password"]); value != "" {
		out["bind_password"] = value
	}
	if domain != "" {
		out["domain"] = domain
	}
	if base != "" {
		out["base"] = base
	}
	if value := strings.TrimSpace(ldapStringArg(args["filter"])); value != "" {
		if _, err := act.ParseFilter(value); err != nil {
			return operation.Spec{}, err
		}
		out["filter"] = value
	}
	return operation.Spec{Type: "ldap", Args: out}, nil
}

func (l *LDAPTool) TranslateResult(op operation.Operation) string { return op.Output }

func (l *LDAPTool) Run(spec operation.Spec) (string, error) {
	return l.RunContext(context.Background(), spec)
}

// RunContext binds, reads and renders.
//
// The order is the house order: the scope record decides whether the host
// may be contacted at all, and only then does anything dial. A refusal
// therefore means nothing was sent.
func (l *LDAPTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	host := strings.TrimSpace(ldapStringArg(spec.Args["host"]))
	if host == "" {
		return "", fmt.Errorf("host is required")
	}
	scheme := strings.ToLower(strings.TrimSpace(ldapStringArg(spec.Args["scheme"])))
	if scheme == "" {
		scheme = "ldap"
	}
	mode := strings.ToLower(strings.TrimSpace(ldapStringArg(spec.Args["mode"])))
	if mode == "" {
		mode = "rootdse"
	}
	port, _ := ldapIntArg(spec.Args["port"])
	if port <= 0 {
		port = ldapDefaultPort(scheme)
	}
	limit, _ := ldapIntArg(spec.Args["limit"])
	if limit <= 0 {
		limit = ldapDefaultLimit
	}
	if limit > ldapMaxLimit {
		limit = ldapMaxLimit
	}
	timeout, _ := ldapIntArg(spec.Args["timeout"])
	if timeout <= 0 {
		timeout = ldapDefaultTimeout
	}
	if timeout > ldapMaxTimeout {
		timeout = ldapMaxTimeout
	}

	// The scope. The engagement names the hosts it covers, and a host it
	// does not name is not contacted.
	if err := act.AuthorizeHost(host, port); err != nil {
		return "", err
	}

	address := net.JoinHostPort(host, strconv.Itoa(port))
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	var client *act.Client
	var err error
	if scheme == "ldaps" {
		client, err = act.DialTLS(ctx, address, time.Duration(timeout)*time.Second, host)
	} else {
		client, err = act.Dial(ctx, "tcp", address, time.Duration(timeout)*time.Second)
	}
	if err != nil {
		return "", err
	}
	defer client.Close()

	bindDN := strings.TrimSpace(ldapStringArg(spec.Args["bind_dn"]))
	bindPassword := ldapStringArg(spec.Args["bind_password"])
	bind := "anonymous"
	if bindDN != "" {
		if err := client.Bind(ctx, bindDN, bindPassword); err != nil {
			return "", err
		}
		bind = "authenticated"
	}

	if mode == "rootdse" {
		info, err := act.Info(ctx, client)
		if err != nil {
			return "", err
		}
		return formatLDAPDomainInfo(info, address, bind), nil
	}

	base, err := ldapBaseDN(spec.Args)
	if err != nil {
		return "", err
	}
	enumeration := act.Mode(mode)
	spec2, err := enumeration.Spec(base)
	if err != nil {
		return "", err
	}
	if extra := strings.TrimSpace(ldapStringArg(spec.Args["filter"])); extra != "" {
		parsed, err := act.ParseFilter(extra)
		if err != nil {
			return "", err
		}
		spec2.Filter = act.AndFilter(spec2.Filter, parsed)
	}
	entries, err := client.Search(ctx, spec2.Base, spec2.Scope, spec2.Filter, spec2.Attributes, limit)
	if err != nil {
		// The entries that were read are real observations, so they are
		// rendered with the error rather than lost.
		if len(entries) > 0 {
			return formatLDAPEntries(entries, mode, spec2.Base, address, bind) + "\n" + err.Error(), nil
		}
		return "", err
	}
	return formatLDAPEntries(entries, mode, spec2.Base, address, bind), nil
}

// ldapBaseDN works out the base DN: the base argument when there is one,
// and otherwise the base derived from the domain.
func ldapBaseDN(args map[string]any) (string, error) {
	if base := strings.TrimSpace(ldapStringArg(args["base"])); base != "" {
		return base, nil
	}
	domain := strings.TrimSpace(ldapStringArg(args["domain"]))
	if domain == "" {
		return "", fmt.Errorf("the mode needs a domain or a base to search under")
	}
	return act.BaseDN(domain)
}

// formatLDAPDomainInfo renders the rootDSE.
func formatLDAPDomainInfo(info act.DomainInfo, address string, bind string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ldap: rootdse on %s (%s bind)\n", address, bind)
	if len(info.NamingContexts) > 0 {
		fmt.Fprintf(&b, "naming contexts: %s\n", strings.Join(info.NamingContexts, ", "))
	}
	if info.DefaultNamingContext != "" {
		fmt.Fprintf(&b, "default naming context: %s\n", info.DefaultNamingContext)
	}
	if info.DNSHostName != "" {
		fmt.Fprintf(&b, "dns host name: %s\n", info.DNSHostName)
	}
	if info.DomainController != "" {
		fmt.Fprintf(&b, "domain controller: %s\n", info.DomainController)
	}
	if info.ForestFunctionalityLevel != 0 {
		fmt.Fprintf(&b, "forest functionality level: %d\n", info.ForestFunctionalityLevel)
	}
	if info.DomainControllerFunctionalityLevel != 0 {
		fmt.Fprintf(&b, "domain controller functionality level: %d\n", info.DomainControllerFunctionalityLevel)
	}
	if len(info.SupportedLDAPVersion) > 0 {
		fmt.Fprintf(&b, "supported ldap versions: %s\n", strings.Join(info.SupportedLDAPVersion, ", "))
	}
	if info.VendorName != "" {
		fmt.Fprintf(&b, "vendor: %s\n", info.VendorName)
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatLDAPEntries renders the entries and their attributes.
//
// The attribute order is sorted, because a directory answers in whatever
// order it likes and a report that reshuffles itself reads like a different
// observation.
func formatLDAPEntries(entries []act.Entry, mode string, base string, address string, bind string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ldap: %s in %s on %s (%s bind)\n", mode, base, address, bind)
	shown := entries
	if len(shown) > ldapMaxRenderedEntries {
		shown = shown[:ldapMaxRenderedEntries]
	}
	for _, entry := range shown {
		fmt.Fprintf(&b, "%s\n", entry.DN)
		names := make([]string, 0, len(entry.Attributes))
		for name := range entry.Attributes {
			names = append(names, name)
		}
		sort.Strings(names)
		for i, name := range names {
			if i >= ldapMaxRenderedAttributes {
				fmt.Fprintf(&b, "  ... %d more attributes\n", len(names)-i)
				break
			}
			values := entry.Attributes[name]
			for j, value := range values {
				if j >= ldapMaxRenderedValues {
					fmt.Fprintf(&b, "  %s: ... %d more values\n", name, len(values)-j)
					break
				}
				fmt.Fprintf(&b, "  %s: %s\n", name, ldapClip(value, ldapMaxValueRunes))
			}
		}
	}
	fmt.Fprintf(&b, "%d entries", len(entries))
	if len(entries) > len(shown) {
		fmt.Fprintf(&b, " (%d listed)", len(shown))
	}
	return strings.TrimRight(b.String(), "\n")
}

// ldapClip bounds one rendered value.
func ldapClip(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "..."
}

// ldapDefaultPort is the port of the scheme when the operator named none.
func ldapDefaultPort(scheme string) int {
	if scheme == "ldaps" {
		return 636
	}
	return 389
}

// ldapStringArg reads a string argument without the zero-value trap.
func ldapStringArg(v any) string {
	s, _ := v.(string)
	return s
}

// ldapIntArg reads an integer argument in both the native and the
// JSON-decoded shape.
func ldapIntArg(v any) (int, bool) {
	switch typed := v.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		if typed == float64(int(typed)) {
			return int(typed), true
		}
	}
	return 0, false
}
