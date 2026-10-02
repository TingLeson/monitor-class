package httpapi

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func mustCIDR(t *testing.T, cidr string) *net.IPNet {
	t.Helper()
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", cidr, err)
	}
	return network
}

func requestFrom(remoteAddr string, headers map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil)
	req.RemoteAddr = remoteAddr
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

func TestClientIPIgnoresHeadersWithoutTrustedProxies(t *testing.T) {
	// The default configuration: no trusted proxies at all.
	resolver := NewClientIPResolver(nil)

	cases := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		want       string
	}{
		{
			name:       "plain peer",
			remoteAddr: "203.0.113.9:51234",
			want:       "203.0.113.9",
		},
		{
			name:       "spoofed X-Forwarded-For",
			remoteAddr: "203.0.113.9:51234",
			headers:    map[string]string{headerXForwardedFor: "1.2.3.4"},
			want:       "203.0.113.9",
		},
		{
			name:       "spoofed X-Real-IP",
			remoteAddr: "203.0.113.9:51234",
			headers:    map[string]string{headerXRealIP: "1.2.3.4"},
			want:       "203.0.113.9",
		},
		{
			name:       "IPv6 peer",
			remoteAddr: "[2001:db8::1]:443",
			headers:    map[string]string{headerXForwardedFor: "1.2.3.4"},
			want:       "2001:db8::1",
		},
		{
			name:       "unparseable peer is reported verbatim",
			remoteAddr: "not-an-address",
			headers:    map[string]string{headerXForwardedFor: "1.2.3.4"},
			want:       "not-an-address",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolver.ClientIP(requestFrom(tc.remoteAddr, tc.headers)); got != tc.want {
				t.Errorf("ClientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClientIPUsesForwardedChainBehindATrustedProxy(t *testing.T) {
	resolver := NewClientIPResolver([]*net.IPNet{mustCIDR(t, "10.0.0.0/8")})

	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{
			name:    "single hop",
			headers: map[string]string{headerXForwardedFor: "198.51.100.7"},
			want:    "198.51.100.7",
		},
		{
			// The client sent a forged prefix; the proxy appended the address it
			// actually saw. Walking right-to-left returns the real client and
			// ignores the forgery.
			name:    "forged prefix in the chain",
			headers: map[string]string{headerXForwardedFor: "1.2.3.4, 198.51.100.7"},
			want:    "198.51.100.7",
		},
		{
			// Two of our own proxies: the last trusted hop is skipped.
			name:    "chained trusted proxies",
			headers: map[string]string{headerXForwardedFor: "198.51.100.7, 10.1.1.1"},
			want:    "198.51.100.7",
		},
		{
			name:    "all hops trusted falls back to the leftmost",
			headers: map[string]string{headerXForwardedFor: "10.1.1.1, 10.1.1.2"},
			want:    "10.1.1.1",
		},
		{
			name:    "junk entries are skipped",
			headers: map[string]string{headerXForwardedFor: "unknown, 198.51.100.7"},
			want:    "198.51.100.7",
		},
		{
			name:    "port in the forwarded value",
			headers: map[string]string{headerXForwardedFor: "198.51.100.7:1234"},
			want:    "198.51.100.7",
		},
		{
			name:    "X-Real-IP when there is no X-Forwarded-For",
			headers: map[string]string{headerXRealIP: "198.51.100.9"},
			want:    "198.51.100.9",
		},
		{
			name:    "nothing usable",
			headers: map[string]string{headerXForwardedFor: "garbage"},
			want:    "10.0.0.5",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolver.ClientIP(requestFrom("10.0.0.5:4000", tc.headers))
			if got != tc.want {
				t.Errorf("ClientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClientIPTrustsOnlyTheConfiguredNetworks(t *testing.T) {
	resolver := NewClientIPResolver([]*net.IPNet{mustCIDR(t, "10.0.0.0/8")})

	// A peer just outside the trusted network must not be able to use headers,
	// even though it is "almost" the proxy.
	got := resolver.ClientIP(requestFrom("11.0.0.5:4000", map[string]string{headerXForwardedFor: "1.2.3.4"}))
	if got != "11.0.0.5" {
		t.Errorf("ClientIP() = %q, want the untrusted peer 11.0.0.5", got)
	}

	// IPv4-mapped IPv6 peers must match an IPv4 rule: a dual-stack listener
	// reports ::ffff:10.0.0.5 and would otherwise silently stop trusting the
	// proxy.
	got = resolver.ClientIP(requestFrom("[::ffff:10.0.0.5]:4000", map[string]string{headerXForwardedFor: "1.2.3.4"}))
	if got != "1.2.3.4" {
		t.Errorf("ClientIP() = %q, want the forwarded address for a mapped IPv4 peer", got)
	}
}

func TestClientAddrReturnsNilForUnparseable(t *testing.T) {
	resolver := NewClientIPResolver(nil)
	if addr := resolver.ClientAddr(requestFrom("203.0.113.9:1234", nil)); addr == nil || addr.String() != "203.0.113.9" {
		t.Errorf("ClientAddr() = %v, want 203.0.113.9", addr)
	}
	if addr := resolver.ClientAddr(requestFrom("garbage", nil)); addr != nil {
		t.Errorf("ClientAddr() = %v, want nil for an unparseable peer", addr)
	}
}

func TestClientIPResolverHandlesNilReceiver(t *testing.T) {
	// A nil resolver must not panic: the access-log middleware holds one, and a
	// probe-only router may construct it from a nil config.
	var resolver *ClientIPResolver
	if got := resolver.ClientIP(requestFrom("203.0.113.9:1", map[string]string{headerXForwardedFor: "1.2.3.4"})); got != "203.0.113.9" {
		t.Errorf("ClientIP() = %q, want the peer address", got)
	}
}

func TestTrustsProxy(t *testing.T) {
	resolver := NewClientIPResolver([]*net.IPNet{mustCIDR(t, "192.168.1.0/24")})
	if !resolver.TrustsProxy(netip.MustParseAddr("192.168.1.10")) {
		t.Error("TrustsProxy(192.168.1.10) = false, want true")
	}
	if resolver.TrustsProxy(netip.MustParseAddr("192.168.2.10")) {
		t.Error("TrustsProxy(192.168.2.10) = true, want false")
	}
}
