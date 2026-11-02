package remotejoin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/testenv"
)

type resolverFunc func(context.Context, *http.Request) (Peer, error)

func (f resolverFunc) Resolve(ctx context.Context, r *http.Request) (Peer, error) { return f(ctx, r) }

type failingStore struct{ AccessDeviceStore }

func isolateStore(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	testenv.SetHome(t, dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
}

func (failingStore) EnsureDevice(context.Context, Peer) (DeviceCredential, error) {
	return DeviceCredential{}, errors.New("secret-canary")
}

func serverConfig() ServerConfig {
	return ServerConfig{BaseURL: "https://node.tailnet.test:9443", ServerVersion: "v3.3-test", Resolver: resolverFunc(func(context.Context, *http.Request) (Peer, error) {
		return Peer{"stable-one", "9007199254740993", "same-name"}, nil
	}), Store: AccessDeviceStore{}, WebSessionReady: func(context.Context) error { return nil }}
}
func registrationRequest(body string) *http.Request {
	r := httptest.NewRequest("POST", "https://node.tailnet.test:9443"+RegistrationPath, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r.WithContext(TrustedServeConnContext(r.Context(), nil))
}
func handlerFor(t *testing.T, c ServerConfig) http.Handler {
	t.Helper()
	h, err := NewRegistrationHandler(c)
	if err != nil {
		t.Fatal("handler configuration rejected")
	}
	return h
}
func assertError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var v struct {
		Error struct{ Code, Message string }
	}
	if json.Unmarshal(w.Body.Bytes(), &v) != nil || w.Code != status || v.Error.Code != code || v.Error.Message == "" {
		t.Fatalf("wrong error classification: status=%d expected=%d code=%s", w.Code, status, code)
	}
	if strings.Contains(w.Body.String(), "secret-canary") {
		t.Fatal("error leaked canary")
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("error is cacheable")
	}
}

func TestRegistrationRetryReturnsSameUnrestrictedCredential(t *testing.T) {
	isolateStore(t)
	h := handlerFor(t, serverConfig())
	var first Registration
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		req := registrationRequest(`{"clientVersion":"v3.3-test"}`)
		req.Host = "evil.test"
		req.Header.Set("X-Forwarded-Host", "evil.test")
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatal("registration did not succeed")
		}
		var got Registration
		if json.Unmarshal(w.Body.Bytes(), &got) != nil {
			t.Fatal("invalid registration JSON")
		}
		if got.ProtocolVersion != 1 || got.ServerVersion != "v3.3-test" || got.Device.ID == "" || got.Gateway.KeyID != got.Device.ID || got.Gateway.Key == "" || got.Device.NodeID != "9007199254740993" {
			t.Fatal("incomplete registration")
		}
		if got.Gateway.BaseURL != "https://node.tailnet.test:9443" || got.Management.LoginURL != "https://node.tailnet.test:9443"+BrowserLoginPath || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("untrusted or cacheable connection URLs")
		}
		if i == 0 {
			first = got
		} else if got.Device.ID != first.Device.ID || got.Gateway.Key != first.Gateway.Key {
			t.Fatal("retry changed credential")
		}
	}
	keys, err := access.List()
	if err != nil || len(keys) != 1 {
		t.Fatal("wrong device count")
	}
	k := keys[0]
	if k.Off || k.ReadOnly || len(k.Models) > 0 || len(k.Accounts) > 0 || k.Limit != nil || len(k.More) > 0 || !k.Expires.IsZero() || k.Issuer != "" || k.Secret != "" {
		t.Fatal("key restrictions or list secret escaped")
	}
}

func TestRegistrationRejectsInputBeforeStore(t *testing.T) {
	isolateStore(t)
	h := handlerFor(t, serverConfig())
	for _, body := range []string{`{"agent":"workbuddy"}`, `{"stableNodeId":"forged"}`, `{"telemetry":true}`, `{"protocolVersion":2}`, `{"clientVersion":null}`, `null`, `[]`, `{} {}`, `{"clientVersion":"a","clientVersion":"b"}`, `{"clientVersion":"` + strings.Repeat("a", 4096) + `"}`} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, registrationRequest(body))
		assertError(t, w, 400, "INVALID_REQUEST")
	}
	p, err := access.ListDevices(context.Background(), "", 100)
	if err != nil || len(p.Items) != 0 {
		t.Fatal("invalid request wrote device")
	}
}

func TestRegistrationRejectsCrossOriginAndWrongMethod(t *testing.T) {
	h := handlerFor(t, serverConfig())
	req := registrationRequest(`{}`)
	req.Header.Set("Origin", "https://evil.test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assertError(t, w, 400, "INVALID_REQUEST")
	req = registrationRequest(`{}`)
	req.Method = "GET"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assertError(t, w, 405, "INVALID_REQUEST")
	if w.Header().Get("Allow") != "POST" {
		t.Fatal("method hint missing")
	}
}

func TestRegistrationErrorsNeverDeliverPartialCredentials(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ServerConfig)
		status int
		code   string
	}{
		{"store", func(c *ServerConfig) { c.Store = failingStore{} }, 503, "STORE_UNAVAILABLE"},
		{"web", func(c *ServerConfig) {
			c.WebSessionReady = func(context.Context) error { return errors.New("secret-canary") }
		}, 503, "WEB_SESSION_UNAVAILABLE"},
		{"no-web", func(c *ServerConfig) { c.WebSessionReady = nil }, 503, "WEB_SESSION_UNAVAILABLE"},
		{"required", func(c *ServerConfig) {
			c.Resolver = resolverFunc(func(context.Context, *http.Request) (Peer, error) { return Peer{}, ErrIdentityRequired })
		}, 401, "TAILSCALE_IDENTITY_REQUIRED"},
		{"unavailable", func(c *ServerConfig) {
			c.Resolver = resolverFunc(func(context.Context, *http.Request) (Peer, error) { return Peer{}, errors.New("secret-canary") })
		}, 503, "TAILSCALE_IDENTITY_UNAVAILABLE"},
		{"bad-xff", func(c *ServerConfig) {
			c.Resolver = resolverFunc(func(context.Context, *http.Request) (Peer, error) { return Peer{}, ErrInvalidRequest })
		}, 400, "INVALID_REQUEST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateStore(t)
			c := serverConfig()
			tc.mutate(&c)
			w := httptest.NewRecorder()
			handlerFor(t, c).ServeHTTP(w, registrationRequest(`{}`))
			assertError(t, w, tc.status, tc.code)
			if strings.Contains(w.Body.String(), `"gateway"`) {
				t.Fatal("partial credential returned")
			}
		})
	}
}

func TestRegistrationRealConnContextDistinguishesTCPFromProtectedListener(t *testing.T) {
	isolateStore(t)
	c := serverConfig()
	c.Resolver = localFixture(t, 200, `{"Node":{"StableID":"node-one","ID":1,"ComputedName":"node"}}`)
	for _, trusted := range []bool{false, true} {
		s := httptest.NewUnstartedServer(handlerFor(t, c))
		if trusted {
			s.Config.ConnContext = func(ctx context.Context, conn net.Conn) context.Context { return TrustedServeConnContext(ctx, conn) }
		}
		s.Start()
		req, _ := http.NewRequest("POST", s.URL+RegistrationPath, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", "100.75.211.11")
		req.Header.Set("Tailscale-User-Login", "forged")
		res, err := s.Client().Do(req)
		if err != nil {
			s.Close()
			t.Fatal("local request failed")
		}
		status := res.StatusCode
		res.Body.Close()
		s.Close()
		want := 401
		if trusted {
			want = 200
		}
		if status != want {
			t.Errorf("trusted=%v status=%d expected=%d", trusted, status, want)
		}
	}
}

func TestRegistrationConcurrentRetriesAndSameNameNodes(t *testing.T) {
	isolateStore(t)
	c := serverConfig()
	c.Resolver = resolverFunc(func(_ context.Context, r *http.Request) (Peer, error) {
		return Peer{r.Header.Get("Test-Node"), "1", "same-name"}, nil
	})
	h := handlerFor(t, c)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := registrationRequest(`{}`)
			node := "node-one"
			if i%2 == 1 {
				node = "node-two"
			}
			req.Header.Set("Test-Node", node)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != 200 {
				t.Error("concurrent registration failed")
			}
		}(i)
	}
	wg.Wait()
	p, err := access.ListDevices(context.Background(), "", 100)
	if err != nil || len(p.Items) != 2 {
		t.Fatal("same-name nodes or retries merged incorrectly")
	}
}

func TestRegistrationRootOriginAndReturnPathBoundaries(t *testing.T) {
	for _, raw := range []string{"http://node.test", "https://user:secret-canary@node.test", "https://node.test/a", "https://node.test?", "https://node.test#", "https://node.test:0", "https://node.test:bad", "https://node.test/%2f", "https://node.test\\evil", "https://[node.test]"} {
		if _, err := RootOrigin(raw); err == nil {
			t.Error("invalid root accepted")
		}
	}
	for _, raw := range []string{"//evil.test", "https://evil.test/", "/a/../b", "/a%2fb", "/a\\b", "/?k=secret-canary", "/a#secret-canary", "/%0d%0aLocation:evil"} {
		if _, err := ValidateReturnPath(raw); err == nil {
			t.Error("unsafe return path accepted")
		}
	}
	if got, err := RootOrigin("https://NODE.test:443/"); err != nil || got != "https://node.test" {
		t.Error("origin normalization failed")
	}
	if got, err := ValidateReturnPath("/api/settings"); err != nil || got != "/api/settings" {
		t.Error("clean return path rejected")
	}
}

func TestRegistrationInputProtocolAndOriginBoundaries(t *testing.T) {
	isolateStore(t)
	h := handlerFor(t, serverConfig())
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
	}{
		{"opaque-origin", func(r *http.Request) { r.Header.Set("Origin", "null") }},
		{"repeated-origin", func(r *http.Request) {
			r.Header["Origin"] = []string{"https://node.tailnet.test:9443", "https://node.tailnet.test:9443"}
		}},
		{"other-port", func(r *http.Request) { r.Header.Set("Origin", "https://node.tailnet.test:9444") }},
		{"query", func(r *http.Request) { r.URL.RawQuery = "key=secret-canary" }},
		{"content-type", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }},
		{"compressed", func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := registrationRequest(`{}`)
			tc.change(req)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			assertError(t, w, 400, "INVALID_REQUEST")
		})
	}
	for _, body := range []string{"", `{"clientVersion":""}`, `{"clientVersion":3}`, `{"clientVersion":"a\nb"}`, `{"clientVersion":"a"`, strings.Repeat(" ", 4096) + `{}`} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, registrationRequest(body))
		assertError(t, w, 400, "INVALID_REQUEST")
	}
	// The byte cap includes whitespace and accepts exactly 4096 bytes.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, registrationRequest(`{}`+strings.Repeat(" ", 4094)))
	if w.Code != 200 {
		t.Fatal("valid body at 4096-byte boundary rejected")
	}
	for _, c := range []ServerConfig{{BaseURL: "https://node.test", ServerVersion: ""}, {BaseURL: "https://user:secret-canary@node.test", ServerVersion: "v3.3"}} {
		if _, err := NewRegistrationHandler(c); err == nil || strings.Contains(err.Error(), "secret-canary") {
			t.Error("unsafe configuration accepted or echoed")
		}
	}
}

func TestRegistrationFailureLogsAndBodiesExcludeUnderlyingSecrets(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	c := serverConfig()
	c.Store = failingStore{}
	w := httptest.NewRecorder()
	handlerFor(t, c).ServeHTTP(w, registrationRequest(`{}`))
	assertError(t, w, 503, "STORE_UNAVAILABLE")
	c = serverConfig()
	c.Resolver = localFixture(t, 500, "secret-canary")
	w = httptest.NewRecorder()
	req := registrationRequest(`{}`)
	req.Header.Set("X-Forwarded-For", "100.75.211.11")
	handlerFor(t, c).ServeHTTP(w, req)
	assertError(t, w, 503, "TAILSCALE_IDENTITY_UNAVAILABLE")
	if strings.Contains(logs.String(), "secret-canary") {
		t.Fatal("underlying failure leaked into logs")
	}
}

func TestRegistrationManagementSignerUnavailableAfterStore(t *testing.T) {
	isolateStore(t)
	c := serverConfig()
	calls := 0
	c.WebSessionReady = func(context.Context) error {
		calls++
		if calls == 1 {
			return nil
		}
		return errors.New("secret-canary")
	}
	w := httptest.NewRecorder()
	handlerFor(t, c).ServeHTTP(w, registrationRequest(`{}`))
	assertError(t, w, 503, "WEB_SESSION_UNAVAILABLE")
	if strings.Contains(w.Body.String(), `"gateway"`) {
		t.Fatal("signer loss delivered partial success")
	}
}
