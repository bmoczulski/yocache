package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestForwardedClient(t *testing.T) {
	var trusted trustedProxies
	for _, v := range []string{"10.0.0.5", "192.168.1.0/24", "fd00::/8"} {
		if err := trusted.add(v); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name, peer string
		xff        []string // one entry per header line
		want       string
	}{
		{"no proxy, no header", "203.0.113.7:4242", nil, "203.0.113.7:4242"},
		{"untrusted peer, forged header ignored", "203.0.113.7:4242", []string{"198.51.100.1"}, "203.0.113.7:4242"},
		{"trusted proxy", "10.0.0.5:5555", []string{"198.51.100.1"}, "198.51.100.1"},
		{"trusted proxy, IPv4-mapped peer", "[::ffff:10.0.0.5]:5555", []string{"198.51.100.1"}, "198.51.100.1"},
		{"client spoofs a hop in front", "10.0.0.5:5555", []string{"1.2.3.4, 198.51.100.1"}, "198.51.100.1"},
		{"chain of two trusted proxies", "10.0.0.5:5555", []string{"198.51.100.1, 192.168.1.20"}, "198.51.100.1"},
		{"header split over two lines", "10.0.0.5:5555", []string{"198.51.100.1", "192.168.1.20"}, "198.51.100.1"},
		{"IPv6 client behind IPv6 proxy", "[fd00::1]:5555", []string{"2001:db8::42"}, "2001:db8::42"},
		{"trusted proxy, no header", "10.0.0.5:5555", nil, "10.0.0.5:5555"},
		{"only trusted hops", "10.0.0.5:5555", []string{"192.168.1.20"}, "192.168.1.20"},
		{"malformed hop stops the walk", "10.0.0.5:5555", []string{"198.51.100.1, garbage, 192.168.1.20"}, "192.168.1.20"},
		{"malformed only", "10.0.0.5:5555", []string{"garbage"}, "10.0.0.5:5555"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got string
			h := withForwardedClient(trusted, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = r.RemoteAddr
			}))
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			req.RemoteAddr = c.peer
			for _, v := range c.xff {
				req.Header.Add("X-Forwarded-For", v)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)
			if got != c.want {
				t.Fatalf("RemoteAddr = %q, want %q", got, c.want)
			}
			if req.RemoteAddr != c.peer {
				t.Fatalf("caller's request was modified: %q", req.RemoteAddr)
			}
		})
	}
}

func TestTrustedProxyFlagValues(t *testing.T) {
	var p trustedProxies
	for _, ok := range []string{"10.0.0.5", "10.0.0.0/8", "::1", "fd00::/8", "10.1.2.3/16"} {
		if err := p.add(ok); err != nil {
			t.Errorf("add(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "proxy.example.com", "10.0.0.0/33", "10.0.0.5:80"} {
		if err := p.add(bad); err == nil {
			t.Errorf("add(%q) accepted", bad)
		}
	}
}
