package proxy

import (
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
)

func TestClientAddrWithTrustedProxies(t *testing.T) {
	trusted, err := ParseTrustedProxies("35.191.0.0/16, 127.0.0.0/8, 192.0.2.10/32")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(nil, nil, zerolog.Nop()).WithTrustedProxies(trusted)

	for _, tc := range []struct {
		name, remote, xff, want string
	}{
		// A caller-prepended entry must not change the per-IP bucket.
		{"load balancer", "35.191.1.2:443", "203.0.113.1, 198.51.100.7, 192.0.2.10", "198.51.100.7"},
		// Peer-forwarded requests arrive over loopback with the origin header;
		// keying on loopback would put all peered traffic in one bucket.
		{"peer tunnel", "127.0.0.1:5010", "203.0.113.1, 198.51.100.7, 192.0.2.10", "198.51.100.7"},
		// A connection that didn't come through our front ends can't choose its address.
		{"direct", "198.51.100.9:443", "203.0.113.1, 192.0.2.10", "198.51.100.9"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tc.remote
		r.Header.Set("X-Forwarded-For", tc.xff)
		if got := h.clientAddr(r); got != tc.want {
			t.Errorf("%s: clientAddr = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestParseTrustedProxiesRejectsOnlySeparators(t *testing.T) {
	if _, err := ParseTrustedProxies(" , "); err == nil {
		t.Fatal("want error for a list with no CIDRs")
	}
}
