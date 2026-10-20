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
var ErrBlockedAddress = errors.New("destination address is in a blocked (private, loopback or link-local) range")

// Resolver resolves host names; *net.Resolver satisfies it.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// DialFunc dials a network address.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",      // "this" network
		"10.0.0.0/8",     // RFC 1918
		"100.64.0.0/10",  // CGNAT
		"127.0.0.0/8",    // loopback
		"169.254.0.0/16", // link-local, cloud metadata
		"172.16.0.0/12",  // RFC 1918
		"192.0.0.0/24",   // IETF protocol assignments
		"192.168.0.0/16", // RFC 1918
		"198.18.0.0/15",  // benchmarking
		"224.0.0.0/4",    // multicast
		"240.0.0.0/4",    // reserved, broadcast
		"::/128",         // unspecified
		"::1/128",        // loopback
		"fc00::/7",       // unique local
		"fe80::/10",      // link-local
		"ff00::/8",       // multicast
		"64:ff9b::/96",   // NAT64 can reach IPv4 private space
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// IsBlockedIP reports whether ip is in a range the executor refuses by
// default (03-security, "HTTP action executor").
func IsBlockedIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	a = a.Unmap()
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
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
		var lastErr error = fmt.Errorf("%w: %s", ErrBlockedAddress, host)
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
