// Package egress provides a hardened outbound HTTP transport that blocks
// connections to loopback, private (RFC 1918 / RFC 4193), link-local, and
// unspecified IP ranges. It exists to close SSRF vectors in framework code
// that makes outbound HTTP calls to attacker-influenced destinations — e.g.
// webhookout's delivery worker POSTing to customer-registered
// webhook_endpoints.url.
//
// The check runs inside the dialer's Control hook, which fires after DNS
// resolution but before the TCP handshake completes, so it inspects the
// actual IP address about to be dialed rather than the original hostname.
// This closes the DNS-rebinding gap: a hostname that resolves to a public IP
// at webhook registration time but a private IP at delivery time is still
// blocked, because the check runs fresh on every dial rather than once at
// registration.
package egress

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// dialTimeout bounds the TCP handshake for a single connection attempt.
const dialTimeout = 10 * time.Second

// GuardedTransport returns an *http.Transport whose dialer refuses to
// complete any TCP connection to a loopback, private, link-local, or
// unspecified address (IPv4 or IPv6, including IPv4-mapped IPv6). It is safe
// to share across goroutines, like any *http.Transport.
func GuardedTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: 30 * time.Second,
		Control:   blockPrivateAddr,
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = dialer.DialContext
	return t
}

// blockPrivateAddr is a net.Dialer.Control hook. The runtime calls it after
// DNS resolution and socket creation but before connect(2), with address
// holding the resolved IP the dialer is about to connect to — so this sees
// the real destination regardless of what hostname the client requested.
func blockPrivateAddr(_ string, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("egress: split host/port %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("egress: unparseable dial address %q", host)
	}
	if Blocked(ip) {
		return fmt.Errorf("egress: destination %s is blocked (loopback/private/link-local/unspecified)", ip)
	}
	return nil
}

// Blocked reports whether ip falls in a loopback, private (RFC 1918 IPv4 /
// RFC 4193 fc00::/7 IPv6), link-local, or unspecified range.
//
// net.IP's IsLoopback/IsPrivate/IsLinkLocal*/IsUnspecified helpers already
// normalize IPv4-mapped IPv6 addresses (e.g. ::ffff:169.254.169.254) via
// To4() before matching, so no separate unwrapping step is needed here.
func Blocked(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsUnspecified()
}
