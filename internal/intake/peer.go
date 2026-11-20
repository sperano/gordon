package intake

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ForwardedForHeader is the header a trusted proxy appends the client
// address it saw to.
const ForwardedForHeader = "X-Forwarded-For"

// Proxies are the addresses of the trusted proxies in front of the
// receiver (the webhook ingress). A request from one of them is
// attributed to the client it forwards, so the per-client slot share
// still separates senders behind the ingress.
type Proxies []netip.Prefix

func (p Proxies) trusts(a netip.Addr) bool {
	for _, pfx := range p {
		if pfx.Contains(a) {
			return true
		}
	}
	return false
}

// Peer is the client address r's slots are charged to: the connection's
// address, or, when that is a trusted proxy, the nearest X-Forwarded-For
// entry that is not one (entries further left are the client's own
// claims, which anyone can forge). A forwarded chain of trusted proxies
// only, or an unparsable entry, leaves the last address that was trusted.
func (p Proxies) Peer(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !p.trusts(addr.Unmap()) {
		return host
	}
	hops := forwarded(r)
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(hops[i])
		if err != nil {
			return host
		}
		if !p.trusts(a.Unmap()) {
			return a.Unmap().String()
		}
		host = hops[i]
	}
	return host
}

// forwarded is the X-Forwarded-For chain, every header line in order.
func forwarded(r *http.Request) []string {
	var hops []string
	for _, line := range r.Header.Values(ForwardedForHeader) {
		for _, h := range strings.Split(line, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hops = append(hops, h)
			}
		}
	}
	return hops
}

// mappedPrefixBits is the length of the ::ffff:0:0/96 prefix an
// IPv4-mapped IPv6 address carries before its IPv4 bits.
const mappedPrefixBits = 96

// ParseProxies parses a comma-separated list of addresses and CIDR
// prefixes.
func ParseProxies(s string) (Proxies, error) {
	var out Proxies
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			a, err := netip.ParseAddr(part)
			if err != nil {
				return nil, err
			}
			out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
			continue
		}
		pfx, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, err
		}
		if pfx.Addr().Is4In6() && pfx.Bits() >= mappedPrefixBits {
			// Peer unmaps connection addresses, so match the IPv4 range.
			pfx = netip.PrefixFrom(pfx.Addr().Unmap(), pfx.Bits()-mappedPrefixBits)
		}
		out = append(out, pfx.Masked())
	}
	return out, nil
}
