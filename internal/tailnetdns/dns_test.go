package tailnetdns

import (
	"context"

	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestFallbackUsesMatchedNodeAndVerifiesTLS(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || r.TLS.ServerName != "example.com" {
			t.Error("original SNI was not retained")
		}
		w.WriteHeader(204)
	}))
	defer s.Close()
	_, port, _ := net.SplitHostPort(s.Listener.Addr().String())
	old := lookup
	defer func() { lookup = old; SetTarget("") }()
	lookup = func(_ context.Context, host string) ([]string, error) {
		if host != "example.com" {
			t.Error("unexpected target")
		}
		return []string{"127.0.0.1"}, nil
	}
	SetTarget(net.JoinHostPort("example.com", port))
	orig := s.Client().Transport.(*http.Transport).Clone()
	var d net.Dialer
	orig.DialContext = Wrap(func(ctx context.Context, network, addr string) (net.Conn, error) {
		if strings.HasPrefix(addr, "example.com:") {
			return nil, &net.DNSError{Err: "no such host", Name: "example.com", IsNotFound: true}
		}
		return d.DialContext(ctx, network, addr)
	})
	c := &http.Client{Transport: orig}
	res, e := c.Get("https://example.com:" + port)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	orig2 := orig.Clone()
	orig2.TLSClientConfig = nil
	c.Transport = orig2
	if _, e = c.Get("https://example.com:" + port); e == nil {
		t.Fatal("untrusted certificate accepted")
	}
}

func TestStatusMappingMatchesOnlyConfiguredName(t *testing.T) {
	nodes := []node{{DNSName: "server.example.ts.net.", TailscaleIPs: []string{"100.100.1.2"}}, {DNSName: "other.example.ts.net.", TailscaleIPs: []string{"100.100.1.3"}}}
	if ip, err := match(nodes, "server.example.ts.net"); err != nil || ip != "100.100.1.2" {
		t.Fatal("known target was not mapped")
	}
	if _, err := match(nodes, "attacker.example.ts.net"); err == nil {
		t.Fatal("unmatched node accepted")
	}
	nodes = append(nodes, node{DNSName: "server.example.ts.net.", TailscaleIPs: []string{"100.100.1.4"}})
	if _, err := match(nodes, "server.example.ts.net"); err == nil {
		t.Fatal("ambiguous node accepted")
	}
}
func TestFallbackNeverUsesOtherHost(t *testing.T) {
	old := lookup
	defer func() { lookup = old; SetTarget("") }()
	SetTarget("known.example.ts.net")
	called := false
	lookup = func(context.Context, string) ([]string, error) { called = true; return []string{"100.64.1.1"}, nil }
	dial := Wrap(func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.DNSError{Err: "not found", Name: "other", IsNotFound: true}
	})
	_, _ = dial(context.Background(), "tcp", "other.example.ts.net:443")
	if called {
		t.Fatal("looked up unconfigured host")
	}
}
func TestNameForIPRequiresUniqueKnownNode(t *testing.T) {
	ip := netip.MustParseAddr("100.100.1.2")
	nodes := []node{{DNSName: "server.example.ts.net.", TailscaleIPs: []string{ip.String()}}}
	if got, e := nameForIP(nodes, ip); e != nil || got != "server.example.ts.net" {
		t.Fatal("known IP not mapped")
	}
	nodes = append(nodes, node{DNSName: "other.example.ts.net.", TailscaleIPs: []string{ip.String()}})
	if _, e := nameForIP(nodes, ip); e == nil {
		t.Fatal("ambiguous IP mapped")
	}
}
