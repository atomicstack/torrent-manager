package main

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAllowlist_AcceptsCIDRsAndBareIPs(t *testing.T) {
	prefixes, err := parseAllowlist([]string{
		"192.0.2.10",
		"10.0.0.0/24",
		"::1",
		"2001:1c00:4697:8100::/64",
		"  ",
	})
	require.NoError(t, err)
	require.Len(t, prefixes, 4, "blank entries are skipped")
	assert.Equal(t, "192.0.2.10/32", prefixes[0].String())
	assert.Equal(t, "10.0.0.0/24", prefixes[1].String())
	assert.Equal(t, "::1/128", prefixes[2].String())
	assert.Equal(t, "2001:1c00:4697:8100::/64", prefixes[3].String())
}

func TestParseAllowlist_RejectsGarbage(t *testing.T) {
	_, err := parseAllowlist([]string{"not-an-ip"})
	require.Error(t, err)
	_, err = parseAllowlist([]string{"10.0.0.0/99"})
	require.Error(t, err)
}

func TestPermits_EmptyIsPassthrough(t *testing.T) {
	a := &allowlistListener{}
	assert.True(t, a.permits(netip.MustParseAddr("1.2.3.4")))
}

func TestPermits_ExactAndCIDR(t *testing.T) {
	prefixes, err := parseAllowlist([]string{"192.0.2.10", "10.0.0.0/24"})
	require.NoError(t, err)
	a := &allowlistListener{allow: prefixes}
	assert.True(t, a.permits(netip.MustParseAddr("192.0.2.10")))
	assert.True(t, a.permits(netip.MustParseAddr("10.0.0.42")))
	assert.False(t, a.permits(netip.MustParseAddr("10.0.1.42")))
	assert.False(t, a.permits(netip.MustParseAddr("198.51.100.10")))
}

func TestPermits_V6AndV4InV6(t *testing.T) {
	prefixes, err := parseAllowlist([]string{
		"192.0.2.10",
		"2001:1c00:4697:8100::/64",
	})
	require.NoError(t, err)
	a := &allowlistListener{allow: prefixes}
	// IPv4-mapped IPv6 must match the v4 prefix after unmap.
	v4in6 := netip.MustParseAddr("::ffff:192.0.2.10")
	assert.True(t, a.permits(v4in6), "ipv4-mapped-ipv6 should match v4 prefix")
	// Any v6 inside the /64 should match.
	assert.True(t, a.permits(netip.MustParseAddr("2001:1c00:4697:8100:dead:beef::1")))
	assert.False(t, a.permits(netip.MustParseAddr("2001:1c00:4697:8200::1")))
}
