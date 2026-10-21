package scan

// ports.go is a TCP port map: which of the common ports answer, which refuse
// and which are silent.
//
// The wall applies to raw TCP exactly as it applies to HTTP. netguard's
// dialer is the only thing that connects, so a port scan cannot reach the
// host's own machine, the hypervisor's management interface or a cloud
// metadata address - the same refusal a probe would get, for the same
// reason.
//
// The three states are the ones an operator can act on:
//
//   - open: something answered. The service name is the usual one for the
//     port, which is a hint and not an identification.
//   - closed: the connection was refused. The port is not listening.
//   - filtered: the connection timed out or the host is unreachable. The
//     port may well be open behind a firewall, and a filtered port is not
//     evidence of a closed one.
//
// The scope gate and the budget are charged per connect attempt, because a
// port scan is a hundred requests to a server, not one.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"ceraa/harness/netguard"
	"ceraa/harness/recon"
)

const (
	defaultTopPorts        = 100
	defaultPortTimeout     = 2
	defaultPortConcurrency = 16
	hardPortConcurrency    = 64
	maxExplicitPorts       = 2000
	// portResolveTimeout bounds the name lookup, so a slow resolver cannot
	// turn a scan into a hang.
	portResolveTimeout = 5 * time.Second
)

// The port states.
const (
	PortOpen     = "open"
	PortClosed   = "closed"
	PortFiltered = "filtered"
)

// DialFunc is the connect seam. The default is netguard's guarded dialer;
// tests inject a fake so nothing leaves the machine.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// PortsOptions bounds one port scan.
type PortsOptions struct {
	// Host is the hostname or IP to scan.
	Host string
	// Ports are explicit ports to try. Empty means the common list.
	Ports []int
	// TopN is how many of the common ports to take. Default 100.
	TopN int
	// Timeout is the per-connect timeout in seconds. Default 2.
	Timeout int
	// Concurrency is the number of parallel connects.
	Concurrency int
	// MaxPorts is the ceiling on ports attempted. 0 means no extra ceiling.
	MaxPorts int
	// UserAgent is recorded for the report; the connect itself carries no
	// HTTP identity.
	UserAgent string
	// Dial is the connect seam. Nil uses the guarded dialer.
	Dial DialFunc
}

// PortResult is one port's state.
type PortResult struct {
	Port    int
	State   string
	Service string
	// Error carries the reason a port was not open, bounded and short.
	Error string
}

// PortsResult is a scan plus the counters a report needs.
type PortsResult struct {
	Ports []PortResult
	// Attempted is how many ports were tried.
	Attempted int
	Open      int
	Closed    int
	Filtered  int
	// Refused counts attempts the scope gate refused before anything was
	// dialled.
	Refused int
	// Skipped is how many ports the ceiling left untried.
	Skipped int
	// Errors holds the first few connect errors, bounded.
	Errors []string
	// Stopped names why the scan ended early, empty when it finished.
	Stopped string
}

// ScanPorts scans the ports of one host and returns the states, ordered by
// port.
func ScanPorts(ctx context.Context, opts PortsOptions) ([]PortResult, error) {
	result, err := ScanPortsDetailed(ctx, opts)
	if err != nil {
		return nil, err
	}
	return result.Ports, nil
}

// ScanPortsDetailed scans the ports and returns the states with the
// counters.
func ScanPortsDetailed(ctx context.Context, opts PortsOptions) (PortsResult, error) {
	host, err := normalizeScanHost(opts.Host)
	if err != nil {
		return PortsResult{}, err
	}
	addresses, err := resolveScanHost(ctx, host)
	if err != nil {
		return PortsResult{}, err
	}
	// The wall, checked before anything is dialled: a host that resolves
	// onto this machine's own network is refused with the address named,
	// which is the useful failure. The dial-time check in netguard still
	// holds the line when a name answers differently a moment later.
	for _, address := range addresses {
		if err := netguard.CheckAddr(address, host); err != nil {
			return PortsResult{}, fmt.Errorf("refused: %v", err)
		}
	}
	ports, err := scanPortList(opts)
	if err != nil {
		return PortsResult{}, err
	}
	skipped := 0
	if opts.MaxPorts > 0 && len(ports) > opts.MaxPorts {
		skipped = len(ports) - opts.MaxPorts
		ports = ports[:opts.MaxPorts]
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultPortTimeout
	}
	dial := opts.Dial
	if dial == nil {
		dial = netguard.DialContext
	}
	workers := opts.Concurrency
	if workers <= 0 {
		workers = defaultPortConcurrency
	}
	if workers > hardPortConcurrency {
		workers = hardPortConcurrency
	}

	state := &portState{max: len(ports)}
	queue := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for port := range queue {
				if ctx.Err() != nil {
					return
				}
				state.probe(ctx, host, port, timeout, dial)
			}
		}()
	}
	for _, port := range ports {
		if ctx.Err() != nil {
			state.setStopped("context cancelled")
			break
		}
		if state.isStopped() {
			break
		}
		select {
		case queue <- port:
		case <-ctx.Done():
		}
	}
	close(queue)
	wg.Wait()

	results := state.resultsSnapshot()
	sort.Slice(results, func(i, j int) bool { return results[i].Port < results[j].Port })
	out := PortsResult{
		Ports:     results,
		Attempted: state.attempted(),
		Open:      state.count(PortOpen),
		Closed:    state.count(PortClosed),
		Filtered:  state.count(PortFiltered),
		Refused:   state.refusedCount(),
		Errors:    state.errorSnapshot(),
		Stopped:   state.stoppedReason(),
	}
	out.Skipped = skipped + len(ports) - out.Attempted
	return out, nil
}

// normalizeScanHost validates the host. A host with a scheme or a path is a
// url, and a url is not something a port scanner can dial.
func normalizeScanHost(raw string) (string, error) {
	host := strings.TrimSpace(raw)
	if host == "" {
		return "", fmt.Errorf("host is required")
	}
	if strings.ContainsAny(host, " \t\r\n") {
		return "", fmt.Errorf("host %q contains whitespace", raw)
	}
	if strings.Contains(host, "://") {
		return "", fmt.Errorf("host %q is a url; give the hostname or the address alone", raw)
	}
	if strings.ContainsAny(host, "/?#@") {
		return "", fmt.Errorf("host %q is not a hostname or an address", raw)
	}
	return host, nil
}

// resolveScanHost turns a host into the addresses to check against the wall.
func resolveScanHost(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return []net.IP{ip}, nil
	}
	lookup, cancel := context.WithTimeout(ctx, portResolveTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(lookup, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	out := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		out = append(out, addr.IP)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	return out, nil
}

// scanPortList builds the list of ports to try: the explicit list when there
// is one, otherwise the head of the common list.
func scanPortList(opts PortsOptions) ([]int, error) {
	if len(opts.Ports) > 0 {
		seen := make(map[int]bool, len(opts.Ports))
		out := make([]int, 0, len(opts.Ports))
		for _, port := range opts.Ports {
			if port < 1 || port > 65535 {
				return nil, fmt.Errorf("port %d is out of range", port)
			}
			if seen[port] {
				continue
			}
			seen[port] = true
			out = append(out, port)
			if len(out) >= maxExplicitPorts {
				break
			}
		}
		sort.Ints(out)
		return out, nil
	}
	top := opts.TopN
	if top <= 0 {
		top = defaultTopPorts
	}
	entries := commonPortEntries()
	if top > len(entries) {
		top = len(entries)
	}
	out := make([]int, 0, top)
	for _, entry := range entries[:top] {
		out = append(out, entry.port)
	}
	return out, nil
}

// portState is the accumulator of a port scan.
type portState struct {
	mu       sync.Mutex
	results  []PortResult
	refused  int
	errors   []string
	stopped  string
	max      int
	attempts int
}

// probe attempts one port and records its state.
func (s *portState) probe(ctx context.Context, host string, port int, timeout int, dial DialFunc) {
	if err := recon.Gate(hostPortURL(host, port)); err != nil {
		s.mu.Lock()
		s.refused++
		s.addErrorLocked(fmt.Sprintf("%s:%d refused by scope: %v", host, port, err))
		s.mu.Unlock()
		return
	}
	if err := recon.Take(1); err != nil {
		s.setStopped(err.Error())
		return
	}
	s.mu.Lock()
	if s.attempts >= s.max {
		s.mu.Unlock()
		return
	}
	s.attempts++
	s.mu.Unlock()

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	dialCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	conn, err := dial(dialCtx, "tcp", addr)
	result := PortResult{Port: port, Service: serviceName(port)}
	if err != nil {
		result.State = classifyDialError(err)
		result.Error = clipDialError(err)
		s.mu.Lock()
		s.results = append(s.results, result)
		s.addErrorLocked(fmt.Sprintf("%s: %s", addr, result.Error))
		s.mu.Unlock()
		return
	}
	if conn != nil {
		conn.Close()
	}
	result.State = PortOpen
	s.mu.Lock()
	s.results = append(s.results, result)
	s.mu.Unlock()
}

// hostPortURL builds the url the scope gate is asked about. The gate speaks
// in urls, and a port scan is a set of requests to one host. The port goes
// in the url so an engagement that names a port token covers that port and
// not the others.
func hostPortURL(host string, port int) string {
	name := host
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && ip.To4() == nil {
		name = "[" + host + "]"
	}
	return "http://" + name + ":" + strconv.Itoa(port)
}

// classifyDialError turns a connect failure into a state.
//
// A refusal means the port is not listening. A timeout means the packets
// went nowhere, which is a firewall rather than a closed port, and the two
// must not be reported as the same thing.
func classifyDialError(err error) string {
	if err == nil {
		return PortOpen
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return PortClosed
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return PortFiltered
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return PortFiltered
	}
	return PortFiltered
}

// clipDialError keeps the reason short enough for one line.
func clipDialError(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if len(text) > 120 {
		text = text[:120]
	}
	return text
}

func (s *portState) addErrorLocked(message string) {
	const maxErrors = 10
	if len(s.errors) >= maxErrors {
		return
	}
	s.errors = append(s.errors, message)
}

func (s *portState) resultsSnapshot() []PortResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PortResult, len(s.results))
	copy(out, s.results)
	return out
}

func (s *portState) attempted() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

func (s *portState) refusedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused
}

func (s *portState) errorSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.errors))
	copy(out, s.errors)
	return out
}

func (s *portState) setStopped(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped == "" {
		s.stopped = reason
	}
}

func (s *portState) stoppedReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

func (s *portState) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped != ""
}

func (s *portState) count(state string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, result := range s.results {
		if result.State == state {
			total++
		}
	}
	return total
}

// ---------------------------------------------------------------------------
// The port table
// ---------------------------------------------------------------------------

// commonPortTable is the port and service-name table, ordered by how often
// the port is open on the internet. TopN takes the head of the list, so the
// order is the ranking.
//
// It is a const string rather than a package-level slice because the package
// holds no mutable state: a shared table could be edited by one caller and
// quietly change for the next one.
//
// The names are the usual ones, not identifications. A port that answers is
// not proven to be running the service its number usually carries.
const commonPortTable = `
80 http
443 https
22 ssh
8080 http-proxy
8443 https-alt
21 ftp
23 telnet
25 smtp
53 domain
110 pop3
143 imap
3306 mysql
5432 postgresql
6379 redis
27017 mongodb
9200 elasticsearch
5601 kibana
2375 docker
3389 ms-wbt-server
1433 ms-sql-s
1521 oracle
5000 upnp
3000 node-http
8000 http-alt
8008 http-alt
8081 http-alt
8888 http-alt
9000 php-fpm
9090 prometheus
11211 memcached
15672 rabbitmq
5672 rabbitmq
4369 epmd
1883 mqtt
5222 xmpp
3690 svn
5984 couchdb
8161 activemq
4848 glassfish
7001 weblogic
7002 weblogic
9042 cassandra
28017 mongo-mongos
6443 kubernetes
10250 kubernetes
2379 etcd
6446 mysql-cluster
1 tcpmux
3 echo
7 echo
9 discard
13 daytime
19 chargen
37 time
79 finger
81 http-alt
88 kerberos
102 s7agent
106 sunrpc
111 rpcbind
113 auth
119 nntp
135 msrpc
137 netbios-ns
138 netbios-dgm
139 netbios-ssn
179 bgp
389 ldap
427 sftp
444 snpp
445 microsoft-ds
465 smtps
513 rlogin
514 shell
515 printer
543 kpasswd
544 kshell
548 afp
554 rtsp
587 submission
631 printer
646 ldap
873 rsync
990 ftps
993 imaps
995 pop3s
1025 nfs
1026 nfs
1027 nfs
1028 nfs
1029 nfs
1110 pop3
1720 h323
1723 pptp
1755 h323
1900 upnp
2000 cisco-sccp
2001 cisco-sccp
2049 nfs
2121 ftp
2717 webex
3128 squid
3986 cisco-ccs
4899 rdp
5009 airport-admin
5051 activesync
5060 sip
5101 cisco-tdp
5190 unknown
5357 wsdapi
5631 pcanywhere
5666 nrpe
5800 vnc
5900 vnc
6000 x11
6001 x11
6646 gnatsd
7070 webproxy
8009 ajp13
9100 jetdirect
9999 cisco-ccs
10000 webmin
32768 filenet
49152 ms-rpc
49153 ms-rpc
49154 ms-rpc
49155 ms-rpc
49156 ms-rpc
49157 ms-rpc
`

// portEntry is one row of the port table.
type portEntry struct {
	port int
	name string
}

// commonPortEntries parses the table.
func commonPortEntries() []portEntry {
	lines := strings.Split(commonPortTable, "\n")
	out := make([]portEntry, 0, len(lines))
	for _, line := range lines {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 {
			continue
		}
		port, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		out = append(out, portEntry{port: port, name: fields[1]})
	}
	return out
}

// serviceName returns the usual name for a port, empty when the port is not
// in the table.
func serviceName(port int) string {
	for _, entry := range commonPortEntries() {
		if entry.port == port {
			return entry.name
		}
	}
	return ""
}
