package intake

import (
	"net/http/httptest"
	"testing"
)

func TestPeerBehindTrustedProxies(t *testing.T) {
	proxies, err := ParseProxies("10.42.0.0/16, 192.0.2.7, ::ffff:10.43.0.0/112")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, remote string
		forwarded    []string
		want         string
	}{
		{"direct client", "198.51.100.1:5000", nil, "198.51.100.1"},
		{"untrusted sender's forged header", "198.51.100.1:5000", []string{"203.0.113.9"}, "198.51.100.1"},
		{"one trusted hop", "10.42.1.5:5000", []string{"203.0.113.9"}, "203.0.113.9"},
		{"forged entry left of the client", "10.42.1.5:5000", []string{"1.1.1.1, 203.0.113.9"}, "203.0.113.9"},
		{"two trusted hops, two header lines", "10.42.1.5:5000", []string{"203.0.113.9", "192.0.2.7"}, "203.0.113.9"},
		{"trusted proxy without header", "10.42.1.5:5000", nil, "10.42.1.5"},
		{"only trusted hops", "10.42.1.5:5000", []string{"10.42.3.3"}, "10.42.3.3"},
		{"unparsable hop", "10.42.1.5:5000", []string{"203.0.113.9, junk"}, "10.42.1.5"},
		{"IPv4-mapped proxy", "[::ffff:10.42.1.5]:5000", []string{"203.0.113.9"}, "203.0.113.9"},
		{"only trusted hops, mapped spelling", "10.42.1.5:5000", []string{"::ffff:10.42.3.3"}, "10.42.3.3"},
		{"direct client, mapped spelling", "[::ffff:198.51.100.1]:5000", nil, "198.51.100.1"},
		{"trusted proxy without header, mapped spelling", "[::ffff:10.42.1.5]:5000", nil, "10.42.1.5"},
		{"unparsable hop after a mapped trusted hop", "10.42.1.5:5000", []string{"203.0.113.9, junk, ::ffff:10.42.3.3"}, "10.42.3.3"},
		{"IPv4 proxy in a mapped CIDR", "10.43.1.5:5000", []string{"203.0.113.9"}, "203.0.113.9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/webhook/github", nil)
			r.RemoteAddr = tc.remote
			for _, f := range tc.forwarded {
				r.Header.Add(ForwardedForHeader, f)
			}
			if got := proxies.Peer(r); got != tc.want {
				t.Fatalf("Peer = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPeerWithoutProxiesIgnoresForwardedFor(t *testing.T) {
	r := httptest.NewRequest("POST", "/webhook/github", nil)
	r.RemoteAddr = "10.42.1.5:5000"
	r.Header.Set(ForwardedForHeader, "203.0.113.9")
	if got := Proxies(nil).Peer(r); got != "10.42.1.5" {
		t.Fatalf("Peer = %q, want the connection's address", got)
	}
}

func TestParseProxiesRefusesGarbage(t *testing.T) {
	for _, in := range []string{"10.0.0.0/33", "not-an-ip", "10.0.0/8"} {
		if _, err := ParseProxies(in); err == nil {
			t.Errorf("ParseProxies(%q) accepted", in)
		}
	}
	if p, err := ParseProxies(" , "); err != nil || len(p) != 0 {
		t.Fatalf("ParseProxies(blank) = %v, %v; want none", p, err)
	}
}
