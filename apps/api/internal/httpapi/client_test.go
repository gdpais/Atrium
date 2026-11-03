package httpapi

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientAddress(t *testing.T) {
	proxies := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8")}
	for _, tc := range []struct {
		name    string
		peer    string
		forward []string
		trusted []netip.Prefix
		want    string
	}{
		{"no proxies: the peer", "198.51.100.7:4000", []string{"203.0.113.9"}, nil, "198.51.100.7"},
		{"untrusted peer: its header is ignored", "198.51.100.7:4000", []string{"203.0.113.9"}, proxies, "198.51.100.7"},
		{"trusted peer: the address it received from", "10.0.0.2:4000", []string{"203.0.113.9"}, proxies, "203.0.113.9"},
		{"a chain of proxies, read from its end", "10.0.0.2:4000", []string{"203.0.113.9, 10.1.0.1, 10.2.0.1"}, proxies, "203.0.113.9"},
		{"what the client wrote first is not believed", "10.0.0.2:4000", []string{"192.0.2.66, 203.0.113.9, 10.1.0.1"}, proxies, "203.0.113.9"},
		{"headers in order", "10.0.0.2:4000", []string{"192.0.2.66", "203.0.113.9", "10.1.0.1"}, proxies, "203.0.113.9"},
		{"garbage stops at the last trusted hop", "10.0.0.2:4000", []string{"203.0.113.9, not-an-ip, 10.1.0.1"}, proxies, "10.1.0.1"},
		{"no header from a trusted peer", "10.0.0.2:4000", nil, proxies, "10.0.0.2"},
		{"every hop trusted", "10.0.0.2:4000", []string{"10.3.0.1, 10.1.0.1"}, proxies, "10.3.0.1"},
		{"mapped IPv4 peer", "[::ffff:198.51.100.7]:4000", nil, nil, "198.51.100.7"},
		{"IPv6 behind an IPv6 proxy", "[fd00::2]:4000", []string{"2001:db8::7"}, proxies, "2001:db8::7"},
		{"unreadable peer", "pipe", nil, proxies, "invalid IP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/v1/session", nil)
			r.RemoteAddr = tc.peer
			for _, value := range tc.forward {
				r.Header.Add("X-Forwarded-For", value)
			}
			if got := clientAddress(r, tc.trusted).String(); got != tc.want {
				t.Fatalf("clientAddress = %s, want %s", got, tc.want)
			}
		})
	}
}
