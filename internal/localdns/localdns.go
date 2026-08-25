// Package localdns makes ".local" (mDNS/Bonjour) targets reachable from a
// CGO-free build.
//
// The release binaries are built with CGO_ENABLED=0 (see
// .github/workflows/release.yml), which forces Go's pure-Go resolver. That
// resolver reads /etc/resolv.conf and sends unicast DNS queries; it never
// calls getaddrinfo, so it never consults /etc/nsswitch.conf and has no mDNS
// support at all. ".local" names live only in mDNS, so they fail to resolve
// even on a machine where `ping nas.local` works perfectly -- ping is a
// separate binary that goes through NSS to Avahi. Same host, two resolvers,
// two different answers.
//
// That matters here because ".local" is the default name for essentially
// every home NAS, printer, and Pi -- exactly the devices this proxy exists to
// reach. nas.local is the worked example in our own comments.
//
// The fix keeps CGO disabled (so static cross-compiled builds still work) and
// resolves ".local" ourselves over multicast, using the pion/mdns client that
// is already in the dependency tree via pion/webrtc's ICE mDNS support.
//
// Resolution order for a ".local" host is deliberately system-first:
//
//  1. A cached mDNS answer, if still fresh.
//  2. The system resolver -- so /etc/hosts overrides keep working, and a
//     machine with a working mDNS path (a CGO build, or systemd-resolved with
//     MulticastDNS enabled) behaves exactly as it does today.
//  3. mDNS over multicast, but only when step 2 failed with a DNS error.
//     A connection refused or timeout is a live host declining us and must
//     not be retried as a name lookup.
package localdns

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/pion/mdns/v2"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const (
	// How long to wait for a multicast answer. Responders on a healthy LAN
	// reply in well under 100ms; this is a ceiling, not a target.
	queryTimeout = 2 * time.Second

	// Bounds on how long an mDNS answer is cached. Record TTLs are honoured
	// between these. The floor keeps us from re-querying on every request
	// when a responder advertises a very short TTL; the ceiling keeps a
	// DHCP address change from pinning us to a dead IP for long.
	minCacheTTL = 30 * time.Second
	maxCacheTTL = 5 * time.Minute
)

// IsLocalName reports whether host is in the mDNS ".local" namespace.
func IsLocalName(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	return strings.HasSuffix(h, ".local")
}

type cacheEntry struct {
	addr    netip.Addr
	expires time.Time
}

// Resolver dials addresses, transparently resolving ".local" hosts over mDNS
// when the system resolver cannot. The zero value is not usable; use New.
type Resolver struct {
	// dial performs the actual connection. Injectable for tests.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// lookup resolves a ".local" name over multicast. Injectable for tests.
	lookup func(ctx context.Context, host string) (netip.Addr, time.Duration, error)

	mu    sync.Mutex
	cache map[string]cacheEntry
}

// New returns a Resolver backed by the system dialer and a real mDNS client.
func New() *Resolver {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return &Resolver{
		dial:   d.DialContext,
		lookup: queryMDNS,
		cache:  make(map[string]cacheEntry),
	}
}

// Default is the process-wide resolver.
var Default = New()

func (r *Resolver) cached(host string) (netip.Addr, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.cache[strings.ToLower(host)]
	if !ok || time.Now().After(e.expires) {
		return netip.Addr{}, false
	}
	return e.addr, true
}

func (r *Resolver) store(host string, addr netip.Addr, ttl time.Duration) {
	if ttl < minCacheTTL {
		ttl = minCacheTTL
	} else if ttl > maxCacheTTL {
		ttl = maxCacheTTL
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[strings.ToLower(host)] = cacheEntry{addr: addr, expires: time.Now().Add(ttl)}
}

func (r *Resolver) invalidate(host string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, strings.ToLower(host))
}

// DialContext is drop-in for http.Transport.DialContext and
// websocket.Dialer.NetDialContext.
func (r *Resolver) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || !IsLocalName(host) {
		return r.dial(ctx, network, addr)
	}

	// A cached answer is tried first, but a stale cache must not be sticky:
	// if the dial fails, drop the entry and fall through to re-resolve.
	if ip, ok := r.cached(host); ok {
		if conn, derr := r.dial(ctx, network, net.JoinHostPort(ip.String(), port)); derr == nil {
			return conn, nil
		}
		r.invalidate(host)
	}

	conn, err := r.dial(ctx, network, addr)
	if err == nil {
		return conn, nil
	}

	// Only a name-resolution failure justifies the multicast fallback.
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		return nil, err
	}

	ip, ttl, lerr := r.lookup(ctx, host)
	if lerr != nil {
		// Report the original DNS failure, noting that mDNS was tried too --
		// otherwise this surfaces as a bare "target unreachable" with no
		// hint that a .local name was involved.
		return nil, fmt.Errorf("%w (mDNS lookup for %q also failed: %v)", err, host, lerr)
	}
	r.store(host, ip, ttl)
	return r.dial(ctx, network, net.JoinHostPort(ip.String(), port))
}

// queryMDNS performs a one-shot multicast query for host.
//
// A fresh listener is opened per lookup rather than held open: lookups are
// rare (the target is pinned per session and the result is cached), and a
// long-lived multicast socket is a lifecycle liability for no gain. Go sets
// SO_REUSEADDR for multicast listens, so this coexists with a running Avahi
// or systemd-resolved.
func queryMDNS(ctx context.Context, host string) (netip.Addr, time.Duration, error) {
	var pktConnV4 *ipv4.PacketConn
	if addr4, err := net.ResolveUDPAddr("udp4", mdns.DefaultAddressIPv4); err == nil {
		if l4, err := net.ListenUDP("udp4", addr4); err == nil {
			pktConnV4 = ipv4.NewPacketConn(l4)
		}
	}
	var pktConnV6 *ipv6.PacketConn
	if addr6, err := net.ResolveUDPAddr("udp6", mdns.DefaultAddressIPv6); err == nil {
		if l6, err := net.ListenUDP("udp6", addr6); err == nil {
			pktConnV6 = ipv6.NewPacketConn(l6)
		}
	}
	// Either family alone is fine; both failing is not.
	if pktConnV4 == nil && pktConnV6 == nil {
		return netip.Addr{}, 0, errors.New("could not bind a multicast socket")
	}

	server, err := mdns.Server(pktConnV4, pktConnV6, &mdns.Config{})
	if err != nil {
		if pktConnV4 != nil {
			pktConnV4.Close()
		}
		if pktConnV6 != nil {
			pktConnV6.Close()
		}
		return netip.Addr{}, 0, err
	}
	defer server.Close()

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	hdr, addr, err := server.QueryAddr(ctx, strings.TrimSuffix(host, "."))
	if err != nil {
		return netip.Addr{}, 0, err
	}
	return addr, time.Duration(hdr.TTL) * time.Second, nil
}

var (
	transportOnce sync.Once
	transport     *http.Transport

	insecureOnce      sync.Once
	insecureTransport *http.Transport
)

// Transport returns a process-wide http.Transport that resolves ".local"
// targets over mDNS. It is a clone of http.DefaultTransport, so proxy and
// timeout behaviour are unchanged; only dialing is intercepted.
//
// Shared deliberately: callers construct an http.Client per request, and
// giving each one its own Transport would discard the connection pool.
func Transport() *http.Transport {
	transportOnce.Do(func() {
		transport = newTransport()
	})
	return transport
}

// InsecureTransport is Transport with certificate verification disabled.
//
// For HTTPS targets on the local network only -- see streamtype.isLocalHost.
// Home devices (NAS boxes, printers, Frigate, OctoPrint) universally serve
// self-signed certificates, so verification cannot succeed and refusing to
// connect buys nothing: the leg being protected is one hop, on a LAN the
// user deliberately reached into, and the internet hop is already covered
// by bitbang's own DTLS. Encryption without authentication still defeats
// passive eavesdropping; it does not defeat an active on-path attacker.
//
// A SEPARATE singleton rather than mutating Transport's TLS config: public
// targets must keep verifying, and a shared mutable config would silently
// disable that for every session in the process. Two pools instead of one
// is the price; correctness is worth more than pool sharing here.
func InsecureTransport() *http.Transport {
	insecureOnce.Do(func() {
		t := newTransport()
		if t.TLSClientConfig == nil {
			t.TLSClientConfig = &tls.Config{}
		} else {
			t.TLSClientConfig = t.TLSClientConfig.Clone()
		}
		t.TLSClientConfig.InsecureSkipVerify = true
		insecureTransport = t
	})
	return insecureTransport
}

func newTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{DialContext: Default.DialContext}
	}
	t := base.Clone()
	t.DialContext = Default.DialContext
	return t
}

// ProbeTCP reports whether host (a "host:port" target) accepts a TCP
// connection, saying nothing about what it speaks. Used to tell "nothing is
// listening" apart from "something is listening but it is not a web server",
// which are very different problems for whoever has to fix them.
func ProbeTCP(ctx context.Context, hostPort string, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := Default.DialContext(ctx, "tcp", hostPort)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ProbeTLS reports whether host (a "host:port" target) answers a TLS
// handshake. Verification is skipped: the question is only "does this
// speak TLS", and a self-signed answer is still a yes.
//
// Used as a fallback after a plaintext probe fails, so the common case
// (plaintext) never pays for it.
func ProbeTLS(ctx context.Context, hostPort string, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Dial through the same mDNS-aware resolver so ".local" HTTPS targets
	// are probed correctly rather than failing name resolution.
	rawConn, err := Default.DialContext(ctx, "tcp", hostPort)
	if err != nil {
		return false
	}
	tlsConn := tls.Client(rawConn, &tls.Config{InsecureSkipVerify: true})
	defer tlsConn.Close()
	return tlsConn.HandshakeContext(ctx) == nil
}
