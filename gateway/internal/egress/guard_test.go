package egress

import (
	"net"
	"net/http"
	"testing"
)

// TestBlockPrivateAddr proves the Control hook blocks loopback, private,
// link-local, and unspecified destinations across both IPv4 and IPv6
// (including IPv4-mapped IPv6), and allows a public IP through.
func TestBlockPrivateAddr(t *testing.T) {
	tests := []struct {
		name    string
		address string
		blocked bool
	}{
		{"IPv4 loopback", "127.0.0.1:443", true},
		{"IPv4 loopback range", "127.5.5.5:80", true},
		{"RFC1918 10.0.0.0/8", "10.0.0.1:443", true},
		{"RFC1918 172.16.0.0/12 low bound", "172.16.0.1:443", true},
		{"RFC1918 172.16.0.0/12 high bound", "172.31.255.254:443", true},
		{"RFC1918 172.15.x is NOT in range", "172.15.255.255:443", false},
		{"RFC1918 172.32.x is NOT in range", "172.32.0.1:443", false},
		{"RFC1918 192.168.0.0/16", "192.168.1.1:443", true},
		{"link-local incl. cloud metadata 169.254.169.254", "169.254.169.254:80", true},
		{"unspecified IPv4 0.0.0.0", "0.0.0.0:443", true},
		{"IPv6 loopback ::1", "[::1]:443", true},
		{"IPv6 unique-local fc00::/7 low", "[fc00::1]:443", true},
		{"IPv6 unique-local fc00::/7 high", "[fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff]:443", true},
		{"IPv6 link-local fe80::/10", "[fe80::1]:443", true},
		{"unspecified IPv6 ::", "[::]:443", true},
		{"IPv4-mapped IPv6 loopback", "[::ffff:127.0.0.1]:443", true},
		{"IPv4-mapped IPv6 metadata", "[::ffff:169.254.169.254]:443", true},
		{"IPv4-mapped IPv6 private", "[::ffff:10.0.0.1]:443", true},
		{"public IPv4", "8.8.8.8:443", false},
		{"public IPv6", "[2001:4860:4860::8888]:443", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := blockPrivateAddr("tcp", tt.address, nil)
			if tt.blocked && err == nil {
				t.Errorf("blockPrivateAddr(%q): expected an error (blocked), got nil", tt.address)
			}
			if !tt.blocked && err != nil {
				t.Errorf("blockPrivateAddr(%q): expected nil (allowed), got error: %v", tt.address, err)
			}
		})
	}
}

// TestBlockPrivateAddr_MalformedAddress verifies the hook fails closed on
// input it cannot parse, rather than silently allowing the dial.
func TestBlockPrivateAddr_MalformedAddress(t *testing.T) {
	if err := blockPrivateAddr("tcp", "not-a-host-port", nil); err == nil {
		t.Error("expected error for address missing a port, got nil")
	}
	if err := blockPrivateAddr("tcp", "not-an-ip:443", nil); err == nil {
		t.Error("expected error for a non-IP host (Control runs post-resolution; a hostname here indicates the dialer did not resolve it), got nil")
	}
}

// TestGuardedTransport_ClonesDefaultTransport verifies GuardedTransport
// returns a usable *http.Transport with its own dialer installed, distinct
// from http.DefaultTransport (so mutating it never affects the shared default).
func TestGuardedTransport_ClonesDefaultTransport(t *testing.T) {
	tr := GuardedTransport()
	if tr == nil {
		t.Fatal("GuardedTransport returned nil")
	}
	if tr.DialContext == nil {
		t.Fatal("GuardedTransport did not install a DialContext")
	}
	def, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is not *http.Transport")
	}
	if tr == def {
		t.Fatal("GuardedTransport must not return the shared http.DefaultTransport")
	}
}

// TestBlocked_DirectIPChecks spot-checks the exported Blocked predicate
// independent of address-string parsing.
func TestBlocked_DirectIPChecks(t *testing.T) {
	if !Blocked(net.ParseIP("192.168.0.1")) {
		t.Error("expected 192.168.0.1 to be blocked")
	}
	if Blocked(net.ParseIP("1.1.1.1")) {
		t.Error("expected 1.1.1.1 to be allowed")
	}
}
