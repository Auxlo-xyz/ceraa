package act

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"ceraa/harness/netguard"
	"ceraa/harness/recon"
)

// ---------------------------------------------------------------------------
// Framing constants
// ---------------------------------------------------------------------------

// The LDAP application tags, from RFC 4511. They are context tags over an
// application class, so the constructed bit is set on the ones that carry a
// sequence.
const (
	tagBindRequest       Tag = 0x60
	tagBindResponse      Tag = 0x61
	tagUnbindRequest     Tag = 0x42
	tagSearchRequest     Tag = 0x63
	tagSearchResultEntry Tag = 0x64
	tagSearchResultDone  Tag = 0x65
	tagSearchResultRef   Tag = 0x73
	tagSimpleAuth        Tag = 0x80
	tagFilterAnd         Tag = 0xa0
	tagFilterOr          Tag = 0xa1
	tagFilterNot         Tag = 0xa2
	tagFilterEquality    Tag = 0xa3
	tagFilterSubstrings  Tag = 0xa4
	tagFilterPresent     Tag = 0x87
	tagSubstringInitial  Tag = 0x80
	tagSubstringAny      Tag = 0x81
	tagSubstringFinal    Tag = 0x82
)

// The bounds. A directory answers with whatever it feels like answering, and
// nothing here trusts it.
const (
	// defaultTimeout bounds one LDAP operation when the caller names none.
	defaultTimeout = 15 * time.Second
	// maxTimeout is the ceiling on a caller-supplied timeout.
	maxTimeout = 120 * time.Second
	// maxEntries is the ceiling on the entries one search may return.
	maxEntries = 1000
	// maxAttributesPerEntry bounds the attributes kept for one entry.
	maxAttributesPerEntry = 64
	// maxValuesPerAttribute bounds the values kept for one attribute.
	maxValuesPerAttribute = 32
	// maxAttributeValueBytes bounds one attribute value.
	maxAttributeValueBytes = 4096
	// maxDNBytes bounds one distinguished name.
	maxDNBytes = 1024
)

// ErrSizeLimit is returned when a search produced more entries than the
// caller allowed. The entries that were read are returned with it, because
// they are real observations and throwing them away would hide what the
// directory was willing to say.
var ErrSizeLimit = errors.New("act: the search returned more entries than the size limit")

// ErrNotAuthorized is returned when the operator's engagement does not name
// the host.
var ErrNotAuthorized = errors.New("act: the engagement does not authorize this host")

// ---------------------------------------------------------------------------
// Scope
// ---------------------------------------------------------------------------

// Scope is the search scope.
type Scope int

// The three search scopes of RFC 4511.
const (
	// ScopeBase is the base object itself.
	ScopeBase Scope = 0
	// ScopeOneLevel is the children of the base object.
	ScopeOneLevel Scope = 1
	// ScopeSubtree is the base object and everything under it.
	ScopeSubtree Scope = 2
)

// ---------------------------------------------------------------------------
// Filters
// ---------------------------------------------------------------------------

// Filter is a search filter.
//
// The interface carries an unexported marker, so a filter can only be built
// by this package: a filter is a piece of a query that goes to a directory,
// and a query assembled from a string by a caller is a query nobody read.
type Filter interface {
	isFilter()
}

type equalityFilter struct {
	attribute string
	value     string
}

type presenceFilter struct {
	attribute string
}

type andFilter struct {
	filters []Filter
}

type orFilter struct {
	filters []Filter
}

type notFilter struct {
	filter Filter
}

type substringFilter struct {
	attribute string
	prefix    string
	middle    string
	suffix    string
}

func (equalityFilter) isFilter()  {}
func (presenceFilter) isFilter()  {}
func (andFilter) isFilter()       {}
func (orFilter) isFilter()        {}
func (notFilter) isFilter()       {}
func (substringFilter) isFilter() {}

// EqualityFilter matches attribute=value.
func EqualityFilter(attribute, value string) Filter {
	return equalityFilter{attribute: attribute, value: value}
}

// PresenceFilter matches the presence of an attribute.
func PresenceFilter(attribute string) Filter {
	return presenceFilter{attribute: attribute}
}

// AndFilter matches when every filter matches.
func AndFilter(filters ...Filter) Filter {
	return andFilter{filters: filters}
}

// OrFilter matches when any filter matches.
func OrFilter(filters ...Filter) Filter {
	return orFilter{filters: filters}
}

// NotFilter matches when the inner filter does not.
func NotFilter(filter Filter) Filter {
	return notFilter{filter: filter}
}

// SubstringFilter matches attribute=prefix*middle*suffix. The middle part is
// the "any" substring, so "svc*sql*" matches a value that starts with svc
// and has sql somewhere after it.
func SubstringFilter(attribute, prefix, middle, suffix string) Filter {
	return substringFilter{attribute: attribute, prefix: prefix, middle: middle, suffix: suffix}
}

// encodeFilter writes the BER of a filter.
//
// The tags follow RFC 4511, whose module is implicitly tagged: the equality
// match replaces the sequence tag of its assertion rather than wrapping it,
// so an equality filter is A3 <len> <attribute> <value> with no sequence
// header in between.
func encodeFilter(f Filter) []byte {
	switch typed := f.(type) {
	case equalityFilter:
		content := append(berOctetString(typed.attribute), berOctetString(typed.value)...)
		return EncodeElement(tagFilterEquality, content)
	case presenceFilter:
		return EncodeElement(tagFilterPresent, []byte(typed.attribute))
	case substringFilter:
		parts := make([]byte, 0, 3)
		if typed.prefix != "" {
			parts = append(parts, EncodeElement(tagSubstringInitial, []byte(typed.prefix))...)
		}
		if typed.middle != "" {
			parts = append(parts, EncodeElement(tagSubstringAny, []byte(typed.middle))...)
		}
		if typed.suffix != "" {
			parts = append(parts, EncodeElement(tagSubstringFinal, []byte(typed.suffix))...)
		}
		substrings := EncodeElement(TagSequence, parts)
		content := append(berOctetString(typed.attribute), substrings...)
		return EncodeElement(tagFilterSubstrings, content)
	case andFilter:
		return EncodeElement(tagFilterAnd, encodeFilterList(typed.filters))
	case orFilter:
		return EncodeElement(tagFilterOr, encodeFilterList(typed.filters))
	case notFilter:
		return EncodeElement(tagFilterNot, encodeFilter(typed.filter))
	default:
		// An unknown filter is a programming error, and the only safe answer
		// is a filter that matches nothing rather than a panic in a tool.
		return EncodeElement(tagFilterPresent, []byte("objectClass"))
	}
}

// encodeFilterList writes the filters of a set.
func encodeFilterList(filters []Filter) []byte {
	out := make([]byte, 0, len(filters)*16)
	for _, f := range filters {
		out = append(out, encodeFilter(f)...)
	}
	return out
}

// ParseFilter reads a filter written the way a directory filter is written:
// (&(objectClass=user)(!(lockedOut=true))). It is what the tool's filter
// argument is parsed with, so the operator's own filter reaches the
// directory as a filter and not as a string somebody typed at a terminal.
//
// The grammar is the part of RFC 4511 that a hand-written filter uses: the
// three combinators, equality, presence and the substring forms. Anything
// else is refused with the reason, because a filter that is quietly
// reinterpreted is a filter that quietly searches the wrong thing.
func ParseFilter(text string) (Filter, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, errors.New("act: the filter is empty")
	}
	parser := &filterParser{input: trimmed}
	filter, err := parser.parseFilter()
	if err != nil {
		return nil, err
	}
	parser.skipSpace()
	if parser.pos != len(parser.input) {
		return nil, fmt.Errorf("act: the filter has trailing text at offset %d", parser.pos)
	}
	return filter, nil
}

// filterParser walks a filter string.
type filterParser struct {
	input string
	pos   int
}

// skipSpace advances past the whitespace.
func (p *filterParser) skipSpace() {
	for p.pos < len(p.input) {
		switch p.input[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

// parseFilter reads one filter.
func (p *filterParser) parseFilter() (Filter, error) {
	p.skipSpace()
	if p.pos >= len(p.input) {
		return nil, errors.New("act: the filter ended early")
	}
	if p.input[p.pos] != '(' {
		return nil, fmt.Errorf("act: the filter has %q where a group was expected", string(p.input[p.pos]))
	}
	return p.parseGroup()
}

// parseGroup reads a parenthesised filter.
func (p *filterParser) parseGroup() (Filter, error) {
	p.pos++ // the opening parenthesis
	p.skipSpace()
	if p.pos >= len(p.input) {
		return nil, errors.New("act: the filter ended inside a group")
	}
	switch p.input[p.pos] {
	case '&':
		p.pos++
		filters, err := p.parseFilterList()
		if err != nil {
			return nil, err
		}
		return AndFilter(filters...), nil
	case '|':
		p.pos++
		filters, err := p.parseFilterList()
		if err != nil {
			return nil, err
		}
		return OrFilter(filters...), nil
	case '!':
		p.pos++
		inner, err := p.parseFilter()
		if err != nil {
			return nil, err
		}
		if err := p.expect(')'); err != nil {
			return nil, err
		}
		return NotFilter(inner), nil
	default:
		return p.parseSimple()
	}
}

// parseFilterList reads the filters of an and or an or group.
func (p *filterParser) parseFilterList() ([]Filter, error) {
	var filters []Filter
	for {
		p.skipSpace()
		if p.pos >= len(p.input) {
			return nil, errors.New("act: the filter ended inside a group")
		}
		if p.input[p.pos] == ')' {
			p.pos++
			if len(filters) == 0 {
				return nil, errors.New("act: the group has no filters in it")
			}
			return filters, nil
		}
		filter, err := p.parseFilter()
		if err != nil {
			return nil, err
		}
		filters = append(filters, filter)
	}
}

// parseSimple reads an attribute test: attribute=value, attribute=*, or the
// substring forms.
func (p *filterParser) parseSimple() (Filter, error) {
	start := p.pos
	for p.pos < len(p.input) && p.input[p.pos] != '=' && p.input[p.pos] != ')' {
		p.pos++
	}
	if p.pos >= len(p.input) || p.input[p.pos] != '=' {
		return nil, fmt.Errorf("act: the filter has no '=' in it at offset %d", start)
	}
	attribute := strings.TrimSpace(p.input[start:p.pos])
	if attribute == "" {
		return nil, fmt.Errorf("act: the filter has an empty attribute at offset %d", start)
	}
	if strings.ContainsAny(attribute, "<>~") {
		return nil, fmt.Errorf("act: the filter for %q uses an operator that is not supported; only = is", attribute)
	}
	p.pos++ // the '='
	valueStart := p.pos
	for p.pos < len(p.input) && p.input[p.pos] != ')' {
		p.pos++
	}
	if p.pos >= len(p.input) {
		return nil, fmt.Errorf("act: the filter ended inside the test for %q", attribute)
	}
	value := p.input[valueStart:p.pos]
	p.pos++ // the closing parenthesis
	if strings.ContainsAny(value, "<>~") {
		return nil, fmt.Errorf("act: the filter for %q uses an operator that is not supported; only = is", attribute)
	}
	if value == "*" {
		return PresenceFilter(attribute), nil
	}
	if strings.Contains(value, "*") {
		parts := strings.Split(value, "*")
		switch len(parts) {
		case 2:
			if strings.HasPrefix(value, "*") {
				return SubstringFilter(attribute, "", "", parts[1]), nil
			}
			return SubstringFilter(attribute, parts[0], "", ""), nil
		case 3:
			return SubstringFilter(attribute, parts[0], parts[1], parts[2]), nil
		default:
			return nil, fmt.Errorf("act: the filter for %q has more than one wildcard", attribute)
		}
	}
	return EqualityFilter(attribute, value), nil
}

// expect consumes one character.
func (p *filterParser) expect(c byte) error {
	p.skipSpace()
	if p.pos >= len(p.input) || p.input[p.pos] != c {
		return fmt.Errorf("act: the filter expected %q at offset %d", string(c), p.pos)
	}
	p.pos++
	return nil
}

// ---------------------------------------------------------------------------
// The client
// ---------------------------------------------------------------------------

// Client is one LDAP connection.
//
// A client is not safe for concurrent use: the LDAP protocol assigns a
// message id per request and the replies come back on the same connection, so
// two searches in flight at once would interleave. The lock is here to make
// that a fact rather than a race.
type Client struct {
	conn    net.Conn
	reader  *bufio.Reader
	timeout time.Duration
	nextID  int

	mu     sync.Mutex
	closed bool
	done   chan struct{}
}

// Entry is one search result.
type Entry struct {
	DN         string
	Attributes map[string][]string
}

// Get returns the first value of an attribute, matched without regard to
// case, because LDAP attribute names are case insensitive and a directory
// that answers "samaccountname" has answered.
func (e Entry) Get(name string) string {
	values := e.GetAll(name)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// GetAll returns every value of an attribute, matched without regard to
// case.
func (e Entry) GetAll(name string) []string {
	for key, values := range e.Attributes {
		if strings.EqualFold(key, name) {
			return values
		}
	}
	return nil
}

// GetInt returns an attribute read as a number. A value that is not a number
// reads as zero: the attribute is a hint, and a hint that does not parse is
// not worth failing a whole enumeration over.
func (e Entry) GetInt(name string) int {
	value := e.Get(name)
	if value == "" {
		return 0
	}
	number, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0
	}
	return number
}

// Dial opens a connection to an LDAP server.
//
// The address is host:port. The host is resolved here and the address that
// was checked is the address that is dialled, so a name that answers
// differently between the check and the connect cannot walk the request onto
// a host the wall refused.
//
// The wall is the egress wall, and it is the wall that refuses an internal
// host the operator's engagement did not open. The scope record is a
// separate question, asked before this: AuthorizeHost.
func Dial(ctx context.Context, network, address string, timeout time.Duration) (*Client, error) {
	return dial(ctx, network, address, timeout, false, "")
}

// DialTLS opens a TLS connection to an LDAP server.
//
// The certificate is verified against the system roots. There is no switch
// that turns that off: the operator points the tool at a host they have
// authorized, and a skip-verify flag in an agent tool is a trap with a
// default setting. A directory whose certificate is not trusted is a
// directory the operator has to install the certificate for, and the error
// says so.
func DialTLS(ctx context.Context, address string, timeout time.Duration, serverName string) (*Client, error) {
	return dial(ctx, "tcp", address, timeout, true, serverName)
}

// dial is the one place a connection is made.
func dial(ctx context.Context, network, address string, timeout time.Duration, useTLS bool, serverName string) (*Client, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return nil, fmt.Errorf("act: the address %q is not host:port: %w", address, err)
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if timeout > maxTimeout {
		timeout = maxTimeout
	}

	conn, err := dialGuarded(ctx, network, host, port, timeout)
	if err != nil {
		return nil, err
	}

	if useTLS {
		name := serverName
		if name == "" {
			name = host
		}
		tlsConn := tls.Client(conn, &tls.Config{ServerName: name, MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("act: the TLS handshake with %s failed: %w", address, err)
		}
		conn = tlsConn
	}

	return &Client{
		conn:    conn,
		reader:  bufio.NewReaderSize(conn, 8192),
		timeout: timeout,
		done:    make(chan struct{}),
	}, nil
}

// dialGuarded resolves, checks and dials, pinning the address it checked.
//
// The pin matters: a name is resolved once here, the address that was
// checked is the address that is dialled, so a name that answers with a
// public address during the check and a private one during the connect
// cannot slip past.
func dialGuarded(ctx context.Context, network, host, port string, timeout time.Duration) (net.Conn, error) {
	dialer := net.Dialer{Timeout: timeout}
	if ip := net.ParseIP(host); ip != nil {
		if err := netguard.CheckAddr(ip, host); err != nil {
			return nil, fmt.Errorf("act: %s: %w", host, err)
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err != nil {
			return nil, fmt.Errorf("act: connect to %s: %w", net.JoinHostPort(host, port), err)
		}
		return conn, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("act: resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("act: %s resolved to no addresses", host)
	}
	var refused error
	for _, resolved := range addrs {
		if err := netguard.CheckAddr(resolved.IP, host); err != nil {
			if refused == nil {
				refused = err
			}
			continue
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(resolved.IP.String(), port))
		if err != nil {
			return nil, fmt.Errorf("act: connect to %s: %w", net.JoinHostPort(host, port), err)
		}
		return conn, nil
	}
	if refused != nil {
		return nil, fmt.Errorf("act: %s: %w", host, refused)
	}
	return nil, fmt.Errorf("act: %s resolved to no address that may be contacted", host)
}

// Close ends the connection.
//
// The unbind is best effort: a server that has already gone away is not a
// failure worth reporting, and the socket is closed either way.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	close(c.done)

	_ = c.conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = c.conn.Write(EncodeElement(tagUnbindRequest, nil))
	return c.conn.Close()
}

// Bind performs a simple bind. An empty password is an anonymous bind.
//
// The password is never logged, never put in an error and never returned:
// the only thing the caller learns from a refused bind is the LDAP result
// code and what it means.
func (c *Client) Bind(ctx context.Context, dn string, password string) error {
	if dn == "" && password != "" {
		// A password with no name is an unauthenticated bind, which every
		// directory refuses. Saying so is more useful than the refusal.
		return errors.New("act: a bind password needs a bind DN")
	}
	stop, err := c.begin(ctx)
	if err != nil {
		return err
	}
	defer stop()

	id := c.nextMessageID()
	if err := c.write(encodeLDAPMessage(id, encodeBindRequest(dn, password))); err != nil {
		return err
	}
	for {
		replyID, tag, content, err := c.readMessage()
		if err != nil {
			return err
		}
		if replyID != id {
			// A reply for another request is not this request's answer.
			continue
		}
		if tag != tagBindResponse {
			return fmt.Errorf("act: the bind reply carries tag 0x%02x, not a bind response", byte(tag))
		}
		code, diagnostic, err := parseLDAPResult(content)
		if err != nil {
			return fmt.Errorf("act: the bind reply is not readable: %w", err)
		}
		if code != 0 {
			return fmt.Errorf("act: the bind was refused: %s (result code %d)%s", ResultCodeText(code), code, diagnosticSuffix(diagnostic))
		}
		return nil
	}
}

// Search runs a search and returns the entries.
//
// The size limit is enforced twice: it goes out in the request so a
// cooperating server stops on its own, and the entries that come back are
// counted so a server that ignores the limit cannot fill the memory. A
// search that hits the limit says so rather than truncating quietly, because
// a truncated list reads like a complete one.
func (c *Client) Search(ctx context.Context, base string, scope Scope, filter Filter, attributes []string, sizeLimit int) ([]Entry, error) {
	if sizeLimit <= 0 {
		sizeLimit = maxEntries
	}
	if sizeLimit > maxEntries {
		sizeLimit = maxEntries
	}
	if filter == nil {
		filter = PresenceFilter("objectClass")
	}
	stop, err := c.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer stop()

	id := c.nextMessageID()
	request := encodeSearchRequest(base, scope, filter, attributes, sizeLimit)
	if err := c.write(encodeLDAPMessage(id, request)); err != nil {
		return nil, err
	}

	entries := make([]Entry, 0, 16)
	limitHit := false
	for {
		replyID, tag, content, err := c.readMessage()
		if err != nil {
			return entries, err
		}
		if replyID != id {
			continue
		}
		switch tag {
		case tagSearchResultEntry:
			if len(entries) >= sizeLimit {
				limitHit = true
				continue
			}
			entry, err := parseSearchEntry(content)
			if err != nil {
				return entries, err
			}
			entries = append(entries, entry)
		case tagSearchResultRef:
			// A referral means the server wants the client to chase another
			// server. Chasing it is a second request to a host nobody
			// authorized, so it is noted and not followed.
			continue
		case tagSearchResultDone:
			code, diagnostic, err := parseLDAPResult(content)
			if err != nil {
				return entries, fmt.Errorf("act: the search result is not readable: %w", err)
			}
			if code == 4 { // sizeLimitExceeded
				limitHit = true
			} else if code != 0 {
				return entries, fmt.Errorf("act: the search failed: %s (result code %d)%s", ResultCodeText(code), code, diagnosticSuffix(diagnostic))
			}
			if limitHit {
				return entries, fmt.Errorf("%w: %d entries", ErrSizeLimit, sizeLimit)
			}
			return entries, nil
		default:
			return entries, fmt.Errorf("act: the search reply carries tag 0x%02x, which is not a search result", byte(tag))
		}
	}
}

// nextMessageID hands out the id of the next request.
func (c *Client) nextMessageID() int {
	c.nextID++
	return c.nextID
}

// begin prepares the connection for one operation.
//
// The deadline comes from the context and from the client timeout,
// whichever is sooner, and a watchdog closes the connection when the context
// is cancelled so a read in flight cannot outlive the call that asked for it.
func (c *Client) begin(ctx context.Context) (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("act: the connection is closed")
	}
	deadline := time.Now().Add(c.timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := c.conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("act: set the deadline: %w", err)
	}
	finished := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// Closing is what unblocks the read; the connection is finished
			// either way, because the caller has already given up on it.
			_ = c.conn.Close()
		case <-finished:
		case <-c.done:
		}
	}()
	return func() { close(finished) }, nil
}

// write sends one message.
func (c *Client) write(message []byte) error {
	if _, err := c.conn.Write(message); err != nil {
		return fmt.Errorf("act: write the request: %w", err)
	}
	return nil
}

// readMessage reads one LDAPMessage and returns its id, its operation tag
// and the operation's content.
func (c *Client) readMessage() (int, Tag, []byte, error) {
	tag, content, err := c.readElement()
	if err != nil {
		return 0, 0, nil, err
	}
	if tag != TagSequence {
		return 0, 0, nil, fmt.Errorf("act: the reply carries tag 0x%02x, not a message", byte(tag))
	}
	return decodeLDAPMessage(content)
}

// readElement reads one complete BER element from the connection.
func (c *Client) readElement() (Tag, []byte, error) {
	header := make([]byte, 0, 8)
	first, err := c.reader.ReadByte()
	if err != nil {
		return 0, nil, fmt.Errorf("act: read the reply: %w", err)
	}
	header = append(header, first)
	lengthByte, err := c.reader.ReadByte()
	if err != nil {
		return 0, nil, fmt.Errorf("act: read the reply length: %w", err)
	}
	header = append(header, lengthByte)

	length := 0
	switch {
	case lengthByte == 0x80:
		return 0, nil, errors.New("act: the reply uses an indefinite length, which is not supported")
	case lengthByte < 0x80:
		length = int(lengthByte)
	default:
		count := int(lengthByte & 0x7f)
		if count > 4 {
			return 0, nil, fmt.Errorf("act: the reply length uses %d octets, over the 4-octet bound", count)
		}
		extra := make([]byte, count)
		if _, err := io.ReadFull(c.reader, extra); err != nil {
			return 0, nil, fmt.Errorf("act: read the reply length: %w", err)
		}
		header = append(header, extra...)
		for _, b := range extra {
			length = length<<8 | int(b)
		}
	}
	if length > maxElementBytes {
		return 0, nil, fmt.Errorf("act: the reply claims %d bytes, over the %d-byte bound", length, maxElementBytes)
	}
	content := make([]byte, length)
	if _, err := io.ReadFull(c.reader, content); err != nil {
		return 0, nil, fmt.Errorf("act: read the reply content: %w", err)
	}
	tag, _, _, err := Decode(append(header, content...))
	if err != nil {
		return 0, nil, fmt.Errorf("act: the reply is not readable: %w", err)
	}
	return tag, content, nil
}

// ---------------------------------------------------------------------------
// Encoding and decoding
// ---------------------------------------------------------------------------

// berOctetString writes an OCTET STRING.
func berOctetString(value string) []byte {
	return EncodeElement(TagOctetString, []byte(value))
}

// berInteger writes an INTEGER.
func berInteger(value int64) []byte {
	return EncodeElement(TagInteger, encodeIntegerContent(value))
}

// berEnumerated writes an ENUMERATED.
func berEnumerated(value int64) []byte {
	return EncodeElement(TagEnumerated, encodeIntegerContent(value))
}

// encodeIntegerContent writes the two's complement content of an INTEGER.
func encodeIntegerContent(value int64) []byte {
	if value == 0 {
		return []byte{0x00}
	}
	negative := value < 0
	out := make([]byte, 0, 8)
	for {
		b := byte(value)
		out = append([]byte{b}, out...)
		value >>= 8
		if negative {
			if value == -1 && b&0x80 != 0 {
				break
			}
			continue
		}
		if value == 0 && b&0x80 == 0 {
			break
		}
	}
	return out
}

// encodeBindRequest writes a BindRequest.
func encodeBindRequest(dn, password string) []byte {
	version := berInteger(3)
	name := berOctetString(dn)
	auth := EncodeElement(tagSimpleAuth, []byte(password))
	content := append(append(version, name...), auth...)
	return EncodeElement(tagBindRequest, content)
}

// encodeSearchRequest writes a SearchRequest.
func encodeSearchRequest(base string, scope Scope, filter Filter, attributes []string, sizeLimit int) []byte {
	content := make([]byte, 0, 64)
	content = append(content, berOctetString(base)...)
	content = append(content, berEnumerated(int64(scope))...)
	// derefAliases: never dereference. A referral is a request to another
	// server, and the operator named one server.
	content = append(content, berEnumerated(0)...)
	content = append(content, berInteger(int64(sizeLimit))...)
	// timeLimit: the server's own default, so a slow query is not cut off
	// by a number this client made up.
	content = append(content, berInteger(0)...)
	// typesOnly: false, the attributes are the point of the search.
	content = append(content, EncodeElement(TagBoolean, []byte{0x00})...)
	content = append(content, encodeFilter(filter)...)

	selection := make([]byte, 0, len(attributes)*24)
	for _, attribute := range attributes {
		selection = append(selection, berOctetString(attribute)...)
	}
	content = append(content, EncodeElement(TagSequence, selection)...)
	return EncodeElement(tagSearchRequest, content)
}

// encodeLDAPMessage wraps a protocol operation in an LDAPMessage.
func encodeLDAPMessage(id int, op []byte) []byte {
	content := append(berInteger(int64(id)), op...)
	return EncodeElement(TagSequence, content)
}

// decodeLDAPMessage reads the message id and the protocol operation out of
// an LDAPMessage's content.
func decodeLDAPMessage(content []byte) (int, Tag, []byte, error) {
	tag, idContent, rest, err := Decode(content)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("act: the message is not readable: %w", err)
	}
	if tag != TagInteger {
		return 0, 0, nil, fmt.Errorf("act: the message starts with tag 0x%02x, not an integer", byte(tag))
	}
	id, err := DecodeInteger(idContent)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("act: the message id is not readable: %w", err)
	}
	opTag, opContent, _, err := Decode(rest)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("act: the protocol operation is not readable: %w", err)
	}
	return int(id), opTag, opContent, nil
}

// parseLDAPResult reads an LDAPResult: the code and the diagnostic message.
//
// The matched DN sits between the two and is not kept: it names a tree the
// caller did not ask about, and the diagnostic is the part an operator reads.
func parseLDAPResult(content []byte) (int, string, error) {
	tag, codeContent, rest, err := Decode(content)
	if err != nil {
		return 0, "", fmt.Errorf("act: the result is not readable: %w", err)
	}
	if tag != TagEnumerated {
		return 0, "", fmt.Errorf("act: the result starts with tag 0x%02x, not an enumerated", byte(tag))
	}
	code, err := DecodeInteger(codeContent)
	if err != nil {
		return 0, "", fmt.Errorf("act: the result code is not readable: %w", err)
	}
	diagnostic := ""
	if _, _, after, err := Decode(rest); err == nil {
		if tag, diagContent, _, err := Decode(after); err == nil && tag == TagOctetString {
			diagnostic = DecodeString(diagContent)
		}
	}
	return int(code), diagnostic, nil
}

// diagnosticSuffix renders the server's own words, when it had any.
func diagnosticSuffix(diagnostic string) string {
	trimmed := strings.TrimSpace(diagnostic)
	if trimmed == "" {
		return ""
	}
	return ": " + trimmed
}

// parseSearchEntry reads a SearchResultEntry.
func parseSearchEntry(content []byte) (Entry, error) {
	tag, dnContent, rest, err := Decode(content)
	if err != nil {
		return Entry{}, fmt.Errorf("act: the search entry is not readable: %w", err)
	}
	if tag != TagOctetString {
		return Entry{}, fmt.Errorf("act: the search entry starts with tag 0x%02x, not a distinguished name", byte(tag))
	}
	dn := DecodeString(dnContent)
	if len(dn) > maxDNBytes {
		dn = dn[:maxDNBytes]
	}
	entry := Entry{DN: dn, Attributes: map[string][]string{}}

	tag, attributesContent, _, err := Decode(rest)
	if err != nil {
		return Entry{}, fmt.Errorf("act: the attributes of %q are not readable: %w", dn, err)
	}
	if tag != TagSequence {
		return Entry{}, fmt.Errorf("act: the attributes of %q start with tag 0x%02x, not a sequence", dn, byte(tag))
	}
	remaining := attributesContent
	count := 0
	for len(remaining) > 0 && count < maxAttributesPerEntry {
		tag, attributeContent, next, err := Decode(remaining)
		if err != nil {
			return entry, nil
		}
		remaining = next
		if tag != TagSequence {
			continue
		}
		name, values, err := parseAttribute(attributeContent)
		if err != nil {
			return entry, nil
		}
		entry.Attributes[name] = values
		count++
	}
	return entry, nil
}

// parseAttribute reads one attribute: its name and its values.
func parseAttribute(content []byte) (string, []string, error) {
	tag, nameContent, rest, err := Decode(content)
	if err != nil {
		return "", nil, fmt.Errorf("act: the attribute is not readable: %w", err)
	}
	if tag != TagOctetString {
		return "", nil, fmt.Errorf("act: the attribute starts with tag 0x%02x, not a name", byte(tag))
	}
	name := DecodeString(nameContent)

	values := make([]string, 0, 4)
	tag, setContent, _, err := Decode(rest)
	if err != nil {
		return name, values, nil
	}
	if tag != TagSet {
		return name, values, nil
	}
	remaining := setContent
	for len(remaining) > 0 && len(values) < maxValuesPerAttribute {
		tag, valueContent, next, err := Decode(remaining)
		if err != nil {
			break
		}
		remaining = next
		if tag != TagOctetString {
			continue
		}
		if len(valueContent) > maxAttributeValueBytes {
			valueContent = valueContent[:maxAttributeValueBytes]
		}
		values = append(values, DecodeString(valueContent))
	}
	return name, values, nil
}

// ---------------------------------------------------------------------------
// Result codes
// ---------------------------------------------------------------------------

// resultCodeText is the human reading of an LDAP result code. The code is in
// the reply, and a code reads as a number to anyone but the protocol
// reference.
var resultCodeText = map[int]string{
	0:  "success",
	1:  "operations error",
	2:  "protocol error",
	3:  "time limit exceeded",
	4:  "size limit exceeded",
	5:  "compare false",
	6:  "compare true",
	7:  "authentication method not supported",
	8:  "stronger authentication required",
	10: "referral",
	11: "admin limit exceeded",
	12: "unavailable critical extension",
	13: "confidentiality required",
	14: "sasl bind in progress",
	16: "no such attribute",
	17: "undefined attribute type",
	18: "inappropriate matching",
	19: "constraint violation",
	20: "attribute or value exists",
	21: "invalid attribute syntax",
	32: "no such object",
	33: "alias problem",
	34: "invalid DN syntax",
	36: "alias dereferencing problem",
	48: "inappropriate authentication",
	49: "invalid credentials",
	50: "insufficient access rights",
	51: "busy",
	52: "unavailable",
	53: "unwilling to perform",
	54: "loop detected",
	60: "naming violation",
	64: "object class violation",
	65: "not allowed on a non-leaf",
	66: "not allowed on an RDN",
	67: "entry already exists",
	68: "object class modifications prohibited",
	71: "affects multiple DSAs",
	80: "other",
}

// ResultCodeText returns the human reading of an LDAP result code.
func ResultCodeText(code int) string {
	if text, ok := resultCodeText[code]; ok {
		return text
	}
	return fmt.Sprintf("ldap result code %d", code)
}

// ---------------------------------------------------------------------------
// Authorization
// ---------------------------------------------------------------------------

// AuthorizeHost reports whether the operator's engagement authorizes
// contacting a host on a port.
//
// recon.Gate is the gate for HTTP and it takes a URL. LDAP and Kerberos are
// not HTTP, so there is no URL to gate, and the decision comes from the same
// record read directly: the engagement's target and its in-scope tokens
// decide, the out-of-scope tokens always win, and a host the record does
// not name is refused.
//
// A private address is a different question, asked by the egress wall at
// connect time rather than here: the engagement may name an internal host
// and still not have opened the range, and the wall's refusal is the one
// that says so.
func AuthorizeHost(host string, port int) error {
	trimmed := strings.TrimSpace(host)
	if trimmed == "" {
		return errors.New("act: a host is required")
	}
	engagement, ok := recon.Active()
	if !ok {
		return fmt.Errorf("%w: no engagement is on record, so nothing authorizes %s:%d", ErrNotAuthorized, trimmed, port)
	}
	if matchesScopeToken(engagement.OutScope, trimmed, port) {
		return fmt.Errorf("%w: %s:%d is on the engagement's out-of-scope list", ErrNotAuthorized, trimmed, port)
	}
	if matchesScopeToken(engagement.InScope, trimmed, port) {
		return nil
	}
	if targetHost := engagementTargetHost(engagement.Target); targetHost != "" && hostCovers(targetHost, trimmed) {
		return nil
	}
	return fmt.Errorf("%w: the engagement does not name %s:%d", ErrNotAuthorized, trimmed, port)
}

// engagementTargetHost reads the host out of an engagement target.
func engagementTargetHost(target string) string {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return ""
	}
	host := trimmed
	if index := strings.Index(host, "://"); index >= 0 {
		host = host[index+3:]
	}
	if index := strings.IndexAny(host, "/?#"); index >= 0 {
		host = host[:index]
	}
	if index := strings.LastIndex(host, ":"); index > 0 && !strings.Contains(host[index:], "]") {
		host = host[:index]
	}
	return strings.ToLower(strings.Trim(host, "[]"))
}

// matchesScopeToken reports whether a token list names this host and port.
//
// A token is a host, a leading-dot host that also covers its subdomains, or
// a host with its own port. A token without a port covers every port: the
// operator named a host, not a port. A token with a port covers only that
// port, because "the directory is on 389" is a narrower statement than
// "the directory is here".
func matchesScopeToken(tokens []string, host string, port int) bool {
	lowered := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	for _, token := range tokens {
		trimmed := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(token), "."))
		if trimmed == "" {
			continue
		}
		tokenHost, tokenPort := splitScopeToken(trimmed)
		if tokenHost == "" {
			continue
		}
		if tokenPort != "" && tokenPort != portString(port) {
			continue
		}
		if hostCovers(tokenHost, lowered) {
			return true
		}
	}
	return false
}

// hostCovers reports whether a token covers a host. A leading dot is
// dropped, because a token that names a host covers that host and its
// subdomains either way.
func hostCovers(token, host string) bool {
	token = strings.TrimPrefix(strings.TrimSpace(token), ".")
	token = strings.TrimSuffix(token, ".")
	if token == "" {
		return false
	}
	return host == token || strings.HasSuffix(host, "."+token)
}

// splitScopeToken reads a token that may carry its own port.
func splitScopeToken(token string) (string, string) {
	if strings.HasPrefix(token, "[") {
		if end := strings.Index(token, "]"); end > 0 {
			rest := token[end+1:]
			if strings.HasPrefix(rest, ":") {
				return token[1:end], rest[1:]
			}
			return token[1:end], ""
		}
		return token, ""
	}
	if index := strings.LastIndex(token, ":"); index >= 0 && isPortDigits(token[index+1:]) {
		return token[:index], token[index+1:]
	}
	return token, ""
}

// isPortDigits reports whether a string is a port number.
func isPortDigits(value string) bool {
	if value == "" || len(value) > 5 {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// portString renders a port for comparison with a token.
func portString(port int) string {
	return strconv.Itoa(port)
}
