package gui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/remotejoin"
)

// This exercises the protected entry with the Host shape observed from Serve;
// a real Tailscale Serve run is still required.
func TestRemoteDevicesProtectedServeOriginAndCookie(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	a, err := access.EnsureDevice(context.Background(), access.DeviceIdentity{StableNodeID: "serve-a", NodeID: "11", DisplayName: "A"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := access.EnsureDevice(context.Background(), access.DeviceIdentity{StableNodeID: "serve-b", NodeID: "12", DisplayName: "B"})
	if err != nil {
		t.Fatal(err)
	}
	session := webSession{cookie: "magpie_web_19443", key: "fake-web-key-0123456789", keep: webCookieAge, secure: true}
	h, err := remoteWebHandler("https://server.example:19443", session, testRemoteResolver{}, Handler(webHost{func() {}}, nil))
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewUnstartedServer(h)
	s.Config.ConnContext = remotejoin.TrustedServeConnContext
	s.Start()
	defer s.Close()
	client := s.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	do := func(method, path, origin string, cookie *http.Cookie) *http.Response {
		t.Helper()
		r, e := http.NewRequest(method, s.URL+path, nil)
		if e != nil {
			t.Fatal(e)
		}
		r.Host = "localhost"
		r.Header.Set("X-Forwarded-Host", "server.example:19443")
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("X-Forwarded-For", "100.75.211.11")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		resp, e := client.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		resp.Body.Close()
		return resp
	}
	if r := do("GET", "/api/remote-devices", "https://server.example:19443", nil); r.StatusCode != 401 {
		t.Fatalf("without management cookie: %d", r.StatusCode)
	}
	login := do("GET", remotejoin.BrowserLoginPath, "", nil)
	if login.StatusCode != 303 || len(login.Cookies()) != 1 {
		t.Fatalf("native login: %d", login.StatusCode)
	}
	ck := login.Cookies()[0]
	if !ck.Secure || !ck.HttpOnly {
		t.Fatal("management cookie lacks secure attributes")
	}
	if r := do("GET", "/api/remote-devices", "https://server.example:19443", ck); r.StatusCode != 200 {
		t.Fatalf("valid HTTPS origin list: %d", r.StatusCode)
	}
	for _, origin := range []string{"http://server.example:19443", "https://foreign.example"} {
		if r := do("DELETE", "/api/remote-devices/"+a.Device.ID, origin, ck); r.StatusCode != 403 {
			t.Fatalf("invalid origin %s: %d", origin, r.StatusCode)
		}
	}
	if _, ok := access.Authenticate(a.Secret); !ok {
		t.Fatal("invalid origin revoked device")
	}
	if r := do("DELETE", "/api/remote-devices/"+a.Device.ID, "https://server.example:19443", ck); r.StatusCode != 204 {
		t.Fatalf("valid HTTPS origin delete: %d", r.StatusCode)
	}
	if _, ok := access.Authenticate(a.Secret); ok {
		t.Fatal("deleted key still authenticates")
	}
	if _, ok := access.Authenticate(b.Secret); !ok {
		t.Fatal("other device affected")
	}
	if r := do("GET", "/api/remote-connection", "https://server.example:19443", ck); r.StatusCode != 404 {
		t.Fatalf("server local-control route: %d", r.StatusCode)
	}
}
