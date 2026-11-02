package httpapi

import (
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

// clientAddress is the address of the client that sent r: the peer's, or,
// when the peer is a trusted proxy, the nearest address in X-Forwarded-For
// that is not one. Proxies append the address they received from, so the
// list is read from its end, and only as far as trusted proxies wrote it:
// what a client puts at its start is never believed.
func clientAddress(r *http.Request, trusted []netip.Prefix) netip.Addr {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	addr := peer.Addr().Unmap()
	if !isTrusted(addr, trusted) {
		return addr
	}
	var hops []string
	for _, value := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(value, ",")...)
	}
	for _, hop := range slices.Backward(hops) {
		next, err := netip.ParseAddr(strings.TrimSpace(hop))
		if err != nil {
			// Not written by a proxy we trust: stop at the last one that was.
			return addr
		}
		addr = next.Unmap()
		if !isTrusted(addr, trusted) {
			return addr
		}
	}
	return addr
}

func isTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	for _, prefix := range trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
