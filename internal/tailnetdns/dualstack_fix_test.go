package tailnetdns

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

func TestDualStackNameForIP(t *testing.T) {
	for _, ips := range [][]string{{"100.100.1.2", "fd7a:115c:a1e0::2"}, {"fd7a:115c:a1e0::2", "100.100.1.2"}} {
		for _, raw := range ips {
			t.Run(ips[0]+"/"+raw, func(t *testing.T) {
				got, err := nameForIP([]node{{DNSName: "server.example.ts.net.", TailscaleIPs: ips}}, netip.MustParseAddr(raw))
				if err != nil || got != "server.example.ts.net" {
					t.Fatalf("unique dual-stack %s: got %q, %v", raw, got, err)
				}
			})
		}
	}
}

func TestFallbackSelectsNetworkFamily(t *testing.T) {
	old := lookup
	defer func() { lookup = old; SetTarget("") }()
	lookup = func(context.Context, string) ([]string, error) {
		return []string{"100.100.1.2", "fd7a:115c:a1e0::2"}, nil
	}
	SetTarget("server.example.ts.net:8443")
	for _, tc := range []struct{ network, want string }{{"tcp4", "100.100.1.2:8443"}, {"tcp6", "[fd7a:115c:a1e0::2]:8443"}} {
		var got string
		dial := Wrap(func(_ context.Context, _ string, addr string) (net.Conn, error) {
			if addr == "server.example.ts.net:8443" {
				return nil, &net.DNSError{Err: "not found"}
			}
			got = addr
			return nil, errors.New("dial failed")
		})
		_, _ = dial(context.Background(), tc.network, "server.example.ts.net:8443")
		if got != tc.want {
			t.Errorf("%s dialed %q, want %q", tc.network, got, tc.want)
		}
	}
}
func TestDualStackRejectsDuplicateOwnership(t *testing.T) {
	a := node{DNSName: "server.example.ts.net.", TailscaleIPs: []string{"100.100.1.2", "fd7a:115c:a1e0::2"}}
	for _, b := range []node{{DNSName: a.DNSName, TailscaleIPs: []string{"100.100.1.3"}}, {DNSName: "other.example.ts.net.", TailscaleIPs: []string{"fd7a:115c:a1e0::2"}}} {
		if _, err := nameForIP([]node{a, b}, netip.MustParseAddr("fd7a:115c:a1e0::2")); err == nil {
			t.Fatal("ambiguous ownership accepted")
		}
	}
}

func TestFallbackDialsActualFamily(t *testing.T) {
	old := lookup
	defer func() { lookup = old; SetTarget("") }()
	for _, tc := range []struct{ network, listen, ip string }{{"tcp4", "127.0.0.1:0", "127.0.0.1"}, {"tcp6", "[::1]:0", "::1"}} {
		t.Run(tc.network, func(t *testing.T) {
			listener, err := net.Listen(tc.network, tc.listen)
			if err != nil {
				t.Skipf("loopback unavailable: %v", err)
			}
			defer listener.Close()
			_, port, _ := net.SplitHostPort(listener.Addr().String())
			target := net.JoinHostPort("family.example.ts.net", port)
			SetTarget(target)
			lookup = func(context.Context, string) ([]string, error) { return []string{"127.0.0.1", "::1"}, nil }
			var dialer net.Dialer
			dial := Wrap(func(ctx context.Context, network, addr string) (net.Conn, error) {
				if addr == target {
					return nil, &net.DNSError{Err: "not found"}
				}
				return dialer.DialContext(ctx, network, addr)
			})
			conn, err := dial(context.Background(), tc.network, target)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			addr := conn.RemoteAddr().(*net.TCPAddr).AddrPort().Addr()
			if addr != netip.MustParseAddr(tc.ip) {
				t.Fatalf("got remote family/address %v", addr)
			}
		})
	}
}
