package integration

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
)

// ErrBlockedAddress is returned when every address of a host is in a
// blocked range and the integration has no private-network opt-in.
var ErrBlockedAddress = errors.New("destination address is in a blocked (private, loopback, link-local or reserved) range")

// Resolver resolves host names; *net.Resolver satisfies it.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// DialFunc dials a network address.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",       // "this" network
		"10.0.0.0/8",      // RFC 1918
		"100.64.0.0/10",   // CGNAT
		"127.0.0.0/8",     // loopback
		"169.254.0.0/16",  // link-local, cloud metadata
		"172.16.0.0/12",   // RFC 1918
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation (TEST-NET-1)
		"192.88.99.0/24",  // deprecated 6to4 relay anycast
		"192.168.0.0/16",  // RFC 1918
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation (TEST-NET-2)
		"203.0.113.0/24",  // documentation (TEST-NET-3)
		"224.0.0.0/4",     // multicast
		"240.0.0.0/4",     // reserved, broadcast
		"::/128",          // unspecified
		"::1/128",         // loopback
		"::/96",           // deprecated IPv4-compatible (::a.b.c.d)
		"::ffff:0:0:0/96", // IPv4-translated (SIIT)
		"64:ff9b::/96",    // NAT64 can reach IPv4 private space
		"64:ff9b:1::/48",  // local-use NAT64 (RFC 8215)
		"100::/64",        // discard-only
		"2001::/32",       // Teredo: tunnels to an embedded IPv4
		"2001:2::/48",     // benchmarking
		"2001:10::/28",    // deprecated ORCHID
		"2001:20::/28",    // ORCHIDv2
		"2001:db8::/32",   // documentation
		"3fff::/20",       // documentation (RFC 9637)
		"5f00::/16",       // SRv6 SIDs
		"fc00::/7",        // unique local
		"fe80::/10",       // link-local
		"fec0::/10",       // deprecated site-local
		"ff00::/8",        // multicast
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// sixToFour is 2002::/16, whose addresses embed an IPv4 address (bits
// 16-47) that a 6to4 relay forwards to.
var sixToFour = netip.MustParsePrefix("2002::/16")

// IsBlockedIP reports whether ip is in a range the executor refuses by
// default (03-security, "HTTP action executor").
func IsBlockedIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	return blockedAddr(a.Unmap())
}

// blockedAddr checks a (IPv4-mapped addresses already unmapped) against
// the blocked ranges, and the IPv4 address a 6to4 address embeds.
func blockedAddr(a netip.Addr) bool {
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	if sixToFour.Contains(a) {
		b := a.As16()
		return blockedAddr(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
	}
	return false
}

// guardedDial resolves the host itself and dials only allowed addresses,
// so the check applies to the address actually connected to (no DNS
// rebinding between check and dial).
func guardedDial(resolver Resolver, dial DialFunc, allowPrivate bool) DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		var ips []net.IP
		if ip := net.ParseIP(host); ip != nil {
			ips = []net.IP{ip}
		} else {
			addrs, err := resolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, a := range addrs {
				ips = append(ips, a.IP)
			}
		}
		lastErr := fmt.Errorf("%w: %s", ErrBlockedAddress, host)
		for _, ip := range ips {
			if !allowPrivate && IsBlockedIP(ip) {
				continue
			}
			c, err := dial(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return c, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
}
