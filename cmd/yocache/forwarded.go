package main

import (
	"fmt"
	"net/http"
	"net/netip"
	"strings"
)

// trustedProxies are the peers whose X-Forwarded-For header is believed when
// working out who a request really came from (--trusted-proxy). The header
// is set by whoever sends the request, so trusting it from anyone would let
// any client put any address into the server's log lines.
type trustedProxies []netip.Prefix

// add parses a CIDR, or a single address meaning just that host.
func (t *trustedProxies) add(v string) error {
	if p, err := netip.ParsePrefix(v); err == nil {
		*t = append(*t, p.Masked())
		return nil
	}
	a, err := netip.ParseAddr(v)
	if err != nil {
		return fmt.Errorf("not an IP address or CIDR: %q", v)
	}
	*t = append(*t, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
	return nil
}

func (t trustedProxies) contains(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range t {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// clientAddr returns the address a request came from, looking through
// trusted proxies. Each proxy appends the address it received the request
// from to X-Forwarded-For, so walking it right to left past trusted hops
// reaches the first address no trusted proxy vouches for. Anything left of
// that was supplied by the client and is ignored. ok is false when the
// request didn't come through a trusted proxy, or the header doesn't say
// anything usable; the caller then keeps r.RemoteAddr as is.
func (t trustedProxies) clientAddr(r *http.Request) (addr string, ok bool) {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil || !t.contains(peer.Addr()) {
		return "", false
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	var leftmost netip.Addr
	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		if hop == "" {
			continue
		}
		a, err := netip.ParseAddr(hop)
		if err != nil {
			// A malformed hop means the chain can't be followed further;
			// stop at the last address a trusted proxy did vouch for.
			break
		}
		if !t.contains(a) {
			return a.Unmap().String(), true
		}
		leftmost = a
	}
	if leftmost.IsValid() {
		return leftmost.Unmap().String(), true // every hop is a trusted proxy
	}
	return "", false
}

// withForwardedClient rewrites r.RemoteAddr to the real client's address
// for requests that arrive through a trusted proxy, so every log line that
// records r.RemoteAddr ("remote=") names the builder, not the proxy.
// Forwarded addresses carry no port, since the header has none.
func withForwardedClient(t trustedProxies, next http.Handler) http.Handler {
	if len(t) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if client, ok := t.clientAddr(r); ok {
			r = r.WithContext(r.Context()) // shallow copy: don't mutate the caller's request
			r.RemoteAddr = client
		}
		next.ServeHTTP(w, r)
	})
}
