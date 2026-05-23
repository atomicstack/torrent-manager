package main

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// allowlistListener wraps a net.Listener and silently closes any inbound
// connection whose peer IP is not covered by one of the configured prefixes.
// Connections are dropped after TCP accept but before TLS sees any bytes, so
// the TLS-handshake-error log spam from drive-by port scans disappears.
//
// An empty prefix list means "accept everything" — backwards-compatible with
// configs that don't set seedctl.yaml's allowlist field.
type allowlistListener struct {
	net.Listener
	allow []netip.Prefix
}

// parseAllowlist converts a list of CIDR strings or bare IP addresses into
// netip.Prefixes. Bare IPs are normalised to a host route (/32 for v4, /128 for v6).
func parseAllowlist(entries []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(entries))
	for _, raw := range entries {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if strings.Contains(raw, "/") {
			p, err := netip.ParsePrefix(raw)
			if err != nil {
				return nil, fmt.Errorf("invalid cidr %q: %w", raw, err)
			}
			out = append(out, p.Masked())
			continue
		}
		ip, err := netip.ParseAddr(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid ip %q: %w", raw, err)
		}
		bits := 32
		if ip.Is6() {
			bits = 128
		}
		out = append(out, netip.PrefixFrom(ip, bits))
	}
	return out, nil
}

func newAllowlistListener(inner net.Listener, prefixes []netip.Prefix) *allowlistListener {
	return &allowlistListener{Listener: inner, allow: prefixes}
}

func (a *allowlistListener) Accept() (net.Conn, error) {
	for {
		c, err := a.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if a.permits(remoteIP(c)) {
			return c, nil
		}
		_ = c.Close()
		// silent drop — logging here would just replace the TLS-error noise
		// with rejection noise. Counters could be added later if needed.
	}
}

// permits returns true when the allowlist is empty (passthrough) OR when ip
// falls inside any configured prefix. Unmaps IPv4-in-IPv6 so a 4-in-6 socket
// matches against a v4 prefix.
func (a *allowlistListener) permits(ip netip.Addr) bool {
	if len(a.allow) == 0 {
		return true
	}
	ip = ip.Unmap()
	for _, p := range a.allow {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// remoteIP pulls the peer IP from a net.Conn, returning the zero netip.Addr
// if the conn isn't a TCP socket (which shouldn't happen for our listener
// but we degrade silently rather than panic).
func remoteIP(c net.Conn) netip.Addr {
	ta, ok := c.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return netip.Addr{}
	}
	addr, _ := netip.AddrFromSlice(ta.IP)
	return addr
}
