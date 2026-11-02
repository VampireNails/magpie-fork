package remotejoin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/tailnetdns"
)

type fakeBinding struct {
	id, key string
	fail    bool
}

func (b *fakeBinding) Bind(id, root, key string) error { b.id, b.key = id, key; return nil }
func (b *fakeBinding) Refresh(context.Context, string) error {
	if b.fail {
		return os.ErrInvalid
	}
	return nil
}
func TestConfigureKeepsProviderAcrossReregistration(t *testing.T) {
	isolateStore(t)
	var root, key = "", "sk-magpie-key-111111111111111111111111111111111111111111111111"
	deviceID := "0123456789abcdef01234567"
	n := 0
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if r.URL.Path != RegistrationPath {
			t.Error("wrong path")
		}
		json.NewEncoder(w).Encode(Registration{ProtocolVersion: 1, ServerVersion: "v3.3-test", Device: RegisteredDevice{ID: deviceID, Peer: Peer{StableNodeID: "node", NodeID: "123", DisplayName: "test-node"}}, Gateway: GatewayConnection{BaseURL: root, KeyID: deviceID, Key: key}, Management: ManagementConnection{BaseURL: root, LoginURL: root + BrowserLoginPath}})
	}))
	defer s.Close()
	root = s.URL
	b := &fakeBinding{}
	c := NewClient(ClientOptions{HTTPClient: s.Client(), Binding: b})
	a, e := c.Configure(context.Background(), root)
	if e != nil {
		t.Fatal(e)
	}
	key = "sk-magpie-key-222222222222222222222222222222222222222222222222"
	deviceID = "abcdefabcdefabcdefabcdef"
	tailnetdns.SetTarget("unrelated.invalid:443")
	z, e := c.Refresh(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if a.ProviderID == "" || a.ProviderID != z.ProviderID || z.DeviceID != deviceID || b.key != key || n != 2 {
		t.Fatal("binding did not retain identity")
	}
	if !tailnetdns.IsTarget(strings.TrimPrefix(root, "https://")) {
		t.Fatal("refresh did not restore the configured DNS target")
	}
	data, e := os.ReadFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "magpie", "remote-connection.json"))
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(data), key) || strings.Contains(string(data), "sk-magpie-key-111111111111111111111111111111111111111111111111") {
		t.Fatal("secret persisted in connection state")
	}
}
func TestConfigureRejectsOriginInjection(t *testing.T) {
	isolateStore(t)
	c := NewClient(ClientOptions{Binding: &fakeBinding{}})
	for _, v := range []string{"https://user:pass@example.test", "https://example.test/path", "https://example.test/?key=secret", "https://example.test/#secret"} {
		if _, e := c.Configure(context.Background(), v); e == nil {
			t.Error("accepted unsafe URL")
		}
	}
}

func TestConfigureRejectsUnsafeResponsesAndRedirects(t *testing.T) {
	isolateStore(t)
	var root string
	mode := "cross"
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("followed redirect") }))
	defer destination.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "redirect":
			http.Redirect(w, r, destination.URL, 307)
		case "cross":
			json.NewEncoder(w).Encode(Registration{ProtocolVersion: 1, ServerVersion: "v3.3-test", Device: RegisteredDevice{ID: "0123456789abcdef01234567", Peer: Peer{StableNodeID: "node", NodeID: "1", DisplayName: "node"}}, Gateway: GatewayConnection{BaseURL: destination.URL, KeyID: "0123456789abcdef01234567", Key: "secret-canary"}, Management: ManagementConnection{BaseURL: root, LoginURL: root + BrowserLoginPath}})
		default:
			fmt.Fprint(w, `{"gateway":{"key":"secret-canary"}`)
		}
	}))
	defer server.Close()
	root = server.URL
	c := NewClient(ClientOptions{HTTPClient: server.Client(), Binding: &fakeBinding{}})
	for _, m := range []string{"cross", "redirect", "malformed"} {
		mode = m
		s, err := c.Configure(context.Background(), root)
		if err == nil || s.State == "connected" || strings.Contains(err.Error(), "secret-canary") {
			t.Errorf("unsafe response accepted or exposed: %s", m)
		}
	}
}
func TestRefreshFailureKeepsLastSuccessfulSync(t *testing.T) {
	isolateStore(t)
	var root string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Registration{ProtocolVersion: 1, ServerVersion: "v3.3-test", Device: RegisteredDevice{ID: "0123456789abcdef01234567", Peer: Peer{StableNodeID: "node", NodeID: "1", DisplayName: "node"}}, Gateway: GatewayConnection{BaseURL: root, KeyID: "0123456789abcdef01234567", Key: "sk-magpie-key-333333333333333333333333333333333333333333333333"}, Management: ManagementConnection{BaseURL: root, LoginURL: root + BrowserLoginPath}})
	}))
	defer server.Close()
	root = server.URL
	binding := &fakeBinding{}
	c := NewClient(ClientOptions{HTTPClient: server.Client(), Binding: binding})
	a, err := c.Configure(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	binding.fail = true
	b, err := c.Refresh(context.Background())
	if err == nil || !a.LastSyncAt.Equal(b.LastSyncAt) || b.State == "connected" {
		t.Fatal("failed refresh changed successful timestamp")
	}
}
func TestCorruptConnectionRemainsUnknown(t *testing.T) {
	isolateStore(t)
	path := connectionPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	c := NewClient(ClientOptions{})
	if _, err := c.Status(context.Background()); err == nil {
		t.Fatal("corrupt file read as empty")
	}
	if _, err := c.Configure(context.Background(), "https://node.tailnet.ts.net"); err == nil {
		t.Fatal("corrupt file overwritten")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "invalid" {
		t.Fatal("corrupt file changed")
	}
}
func TestSwitchRequiresRestore(t *testing.T) {
	isolateStore(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	c := NewClient(ClientOptions{Binding: &fakeBinding{}, HTTPClient: server.Client()})
	_, _ = c.Configure(context.Background(), server.URL)
	if _, e := c.Configure(context.Background(), "https://two.tailnet.ts.net"); e == nil {
		t.Fatal("server switched without restore hook")
	}
	s, _ := c.Status(context.Background())
	if s.ServerURL != server.URL {
		t.Fatal("server changed")
	}
}

func TestConcurrentRefreshUsesOneProvider(t *testing.T) {
	isolateStore(t)
	var root string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Registration{ProtocolVersion: 1, ServerVersion: "v3.3-test", Device: RegisteredDevice{ID: "0123456789abcdef01234567", Peer: Peer{StableNodeID: "node", NodeID: "1", DisplayName: "node"}}, Gateway: GatewayConnection{BaseURL: root, KeyID: "0123456789abcdef01234567", Key: "sk-magpie-key-333333333333333333333333333333333333333333333333"}, Management: ManagementConnection{BaseURL: root, LoginURL: root + BrowserLoginPath}})
	}))
	defer server.Close()
	root = server.URL
	binding := &fakeBinding{}
	a := NewClient(ClientOptions{HTTPClient: server.Client(), Binding: binding})
	b := NewClient(ClientOptions{HTTPClient: server.Client(), Binding: binding})
	first, e := a.Configure(context.Background(), root)
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	errc := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := a
			if i%2 != 0 {
				c = b
			}
			s, e := c.Refresh(context.Background())
			if e == nil && s.ProviderID != first.ProviderID {
				e = errors.New("provider identity changed")
			}
			errc <- e
		}(i)
	}
	wg.Wait()
	close(errc)
	for e := range errc {
		if e != nil {
			t.Fatal(e)
		}
	}
}

func TestConfigureRejectsUntrustedCertificate(t *testing.T) {
	isolateStore(t)
	s := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("untrusted server reached") }))
	defer s.Close()
	c := NewClient(ClientOptions{HTTPClient: &http.Client{}, Binding: &fakeBinding{}})
	status, err := c.Configure(context.Background(), s.URL)
	if err == nil || status.LastErrorCode != "tls_certificate_invalid" {
		t.Fatal("certificate failure was not reported")
	}
}
