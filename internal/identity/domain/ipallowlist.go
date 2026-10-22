package domain

import (
	"fmt"
	"net/netip"
)

// ParseIPAllowlist parses each entry as a CIDR (e.g. "203.0.113.0/24")
// or a bare IP address, which is treated as a single-address prefix
// (/32 for IPv4, /128 for IPv6) — the common case of allowlisting one
// address without making the caller spell out its full prefix length.
func ParseIPAllowlist(entries []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(entries))
	for _, entry := range entries {
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			addr, addrErr := netip.ParseAddr(entry)
			if addrErr != nil {
				return nil, fmt.Errorf("domain: %q is not a valid IP address or CIDR", entry)
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}
