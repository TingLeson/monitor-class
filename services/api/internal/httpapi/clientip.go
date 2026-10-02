package httpapi

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ClientIPResolver answers "which address is this request really from?" under an
// explicit trust policy.
//
// WHY this exists instead of http.Request.RemoteAddr or gin's c.ClientIP():
//
//   - RemoteAddr is the TCP peer. Behind a reverse proxy that is the proxy, so
//     every client would share one rate-limit bucket and the audit log would name
//     the load balancer instead of the caller.
//   - X-Forwarded-For / X-Real-IP are set by the *client* unless a proxy rewrites
//     them. Trusting them unconditionally means any caller can pick their own IP:
//     rotate the header and every rate limit becomes a fresh bucket, and the
//     sessions.ip column becomes fiction. Both the login limiter and the access
//     log key on this value, so a single policy has to feed both.
//
// The policy implemented here is therefore: believe proxy headers ONLY when the
// TCP peer is inside TRUSTED_PROXIES (empty by default = believe nobody). It also
// walks the forwarded chain from right to left, skipping hops that are themselves
// trusted proxies, so a client-supplied prefix in the header cannot override the
// address the proxy actually observed.
type ClientIPResolver struct {
	trusted []*net.IPNet
}

// NewClientIPResolver builds a resolver from the configured trusted networks.
// A nil or empty list means "no proxy headers are trusted".
func NewClientIPResolver(trusted []*net.IPNet) *ClientIPResolver {
	return &ClientIPResolver{trusted: trusted}
}

// ClientIP returns the effective client address as a string, suitable for log
// fields and rate-limit keys. It never returns a header value from an untrusted
// peer.
func (r *ClientIPResolver) ClientIP(req *http.Request) string {
	if req == nil {
		return ""
	}
	peer, peerAddr := remoteHost(req.RemoteAddr)
	// An unparseable RemoteAddr means we cannot make a trust decision, so we
	// trust nothing and report the raw peer.
	if peerAddr == nil || !r.trusts(*peerAddr) {
		return peer
	}

	if forwarded, ok := parseForwardedChain(req.Header.Get(headerXForwardedFor), r.trusts); ok {
		return forwarded
	}
	// X-Real-IP is a single value set by nginx and friends; it is only consulted
	// when there is no usable X-Forwarded-For and the peer is already trusted.
	if realIP := normalizeAddr(req.Header.Get(headerXRealIP)); realIP != nil {
		return realIP.String()
	}
	return peer
}

// ClientAddr is ClientIP as a typed address, for the sessions.ip column. It
// returns nil when nothing parseable is available, which the caller stores as
// SQL NULL rather than inventing an address.
func (r *ClientIPResolver) ClientAddr(req *http.Request) *netip.Addr {
	ip := r.ClientIP(req)
	if ip == "" {
		return nil
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return nil
	}
	return &addr
}

// TrustsProxy reports whether addr is inside the trusted proxy list. Exported for
// tests and for diagnostics.
func (r *ClientIPResolver) TrustsProxy(addr netip.Addr) bool { return r.trusts(addr) }

func (r *ClientIPResolver) trusts(addr netip.Addr) bool {
	if r == nil || len(r.trusted) == 0 {
		return false
	}
	// Unmap IPv4-in-IPv6 (::ffff:10.0.0.1) so a dual-stack listener does not
	// silently fail to match an IPv4 proxy rule.
	addr = addr.Unmap()
	for _, network := range r.trusted {
		if network == nil {
			continue
		}
		if network.Contains(addr.AsSlice()) {
			return true
		}
	}
	return false
}

const (
	headerXForwardedFor = "X-Forwarded-For"
	headerXRealIP       = "X-Real-IP"
)

// parseForwardedChain returns the client address from an X-Forwarded-For value.
//
// The chain is walked from RIGHT to LEFT: the rightmost entry is the one the
// trusted proxy appended (the address it actually saw), so it is the most
// trustworthy. Entries that are themselves trusted proxies are skipped, and the
// first untrusted address wins. If every entry is a trusted proxy, the leftmost
// one is the best available answer.
//
// Reading the chain left-to-right instead — the naive implementation — returns
// whatever the client put in the header first, which is exactly the spoofing
// vector this resolver exists to close.
func parseForwardedChain(value string, trusted func(netip.Addr) bool) (string, bool) {
	if strings.TrimSpace(value) == "" {
		return "", false
	}
	parts := strings.Split(value, ",")
	// Iterate in reverse: the rightmost entry is the one our trusted proxy
	// appended, so it is the first that can be believed.
	var leftmost string
	for i := len(parts) - 1; i >= 0; i-- {
		addr := normalizeAddr(parts[i])
		if addr == nil {
			// Junk entries are skipped rather than trusted or fatal: a proxy may
			// append "unknown", and refusing to serve would be worse than using
			// the remaining evidence.
			continue
		}
		leftmost = addr.String()
		if !trusted(*addr) {
			return addr.String(), true
		}
	}
	if leftmost != "" {
		return leftmost, true
	}
	return "", false
}

// remoteHost splits "host:port" (including "[::1]:port") into host and address.
func remoteHost(remoteAddr string) (string, *netip.Addr) {
	host := strings.TrimSpace(remoteAddr)
	if host == "" {
		return "", nil
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	// A bare IPv6 address without brackets is not something RemoteAddr produces,
	// but it can appear in tests; keep it as-is and let ParseAddr decide.
	if addr := normalizeAddr(host); addr != nil {
		return addr.String(), addr
	}
	return host, nil
}

// normalizeAddr parses an address that may carry a port, brackets, surrounding
// whitespace or an IPv6 zone.
func normalizeAddr(raw string) *netip.Addr {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil
	}
	// "[2001:db8::1]:443" and "203.0.113.9:51234" both appear in the wild.
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	value = strings.Trim(value, "[]")
	if zone := strings.IndexByte(value, '%'); zone >= 0 {
		value = value[:zone]
	}
	addr, err := netip.ParseAddr(value)
	if err != nil {
		return nil
	}
	addr = addr.Unmap()
	return &addr
}
