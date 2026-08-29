// Package allowlist restricts which host:port targets a listener will reach
// on a requester's behalf.
//
// Both the proxy and TCP forwarding let the far side name a target, and
// without a restriction that means every host:port the listener can reach --
// so a link handed out for one service also reaches the rest of the network.
// The same matching serves both, so "allowed" means one thing rather than two.
package allowlist

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// entry is one allowed target. port 0 means "any port on this host".
type entry struct {
	host string
	port int
}

// List is a set of allowed targets. The zero value allows everything, which
// is what a listener started without any -allow flag does.
type List []entry

// Parse builds a List from "host:port" or "host" specs. A spec without a
// port allows any port on that host.
func Parse(specs []string) (List, error) {
	var l List
	for _, spec := range specs {
		e, err := parseEntry(spec)
		if err != nil {
			return nil, err
		}
		l = append(l, e)
	}
	return l, nil
}

func parseEntry(spec string) (entry, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return entry{}, fmt.Errorf("empty target")
	}
	host, portStr, err := net.SplitHostPort(spec)
	if err != nil {
		// No port: allow every port on this host. Bare IPv6 has to be
		// bracketed to be told apart from host:port, so require that.
		if strings.Count(spec, ":") > 0 && !strings.HasPrefix(spec, "[") {
			return entry{}, fmt.Errorf("invalid target %q (bracket IPv6 hosts: [::1]:22)", spec)
		}
		return entry{host: normalizeHost(spec)}, nil
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return entry{}, fmt.Errorf("invalid port in %q", spec)
	}
	if host == "" {
		return entry{}, fmt.Errorf("invalid target %q (no host)", spec)
	}
	return entry{host: normalizeHost(host), port: port}, nil
}

// normalizeHost lowercases, drops a trailing root dot, and strips IPv6
// brackets, so the same host written three ways compares equal.
func normalizeHost(h string) string {
	h = strings.Trim(strings.TrimSpace(h), "[]")
	h = strings.TrimSuffix(h, ".")
	return strings.ToLower(h)
}

// Empty reports whether the list restricts nothing.
func (l List) Empty() bool { return len(l) == 0 }

// Permits reports whether host:port may be reached. An empty list permits
// everything. port 0 means the requester did not name one, which matches
// only an entry that does not name one either.
//
// Matching is on the target as written, with no name resolution: resolving
// would check a name at one moment and dial it at another, and the two can
// disagree. The cost is that allowing a host by name does not allow it by
// address, which is the safe direction to be wrong in.
func (l List) Permits(host string, port int) bool {
	if l.Empty() {
		return true
	}
	h := normalizeHost(host)
	for _, e := range l {
		if e.host != h {
			continue
		}
		if e.port == 0 || e.port == port {
			return true
		}
	}
	return false
}

// PermitsTarget is Permits for a "host:port" or "host" string.
func (l List) PermitsTarget(target string) bool {
	if l.Empty() {
		return true
	}
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return l.Permits(target, 0)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return false
	}
	return l.Permits(host, port)
}

// String renders the list for error messages and the console listing.
func (l List) String() string {
	if l.Empty() {
		return ""
	}
	out := make([]string, 0, len(l))
	for _, e := range l {
		host := e.host
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		if e.port == 0 {
			out = append(out, host+":*")
			continue
		}
		out = append(out, host+":"+strconv.Itoa(e.port))
	}
	return strings.Join(out, ", ")
}
