package gui

import (
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/remotejoin"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type bridgeTransport func(*http.Request) (*http.Response, error)

func (f bridgeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRemoteServerHTTPNativeManagementAndGateway(t *testing.T) {
	onePortHome(t)
	gatewayKey := onePortKey(t)
	old := served.Swap(gateway.New())
	t.Cleanup(func() { served.Store(old) })
	who := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"Node":{"StableID":"node-one","ID":123,"ComputedName":"fixture"}}`)
	}))
	defer who.Close()
	c := &http.Client{Transport: bridgeTransport(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.URL.Scheme = "http"
		r.URL.Host = who.Listener.Addr().String()
		return who.Client().Transport.RoundTrip(r)
	})}
	resolver := remotejoin.NewLocalAPIResolver(c, time.Second)
	page := Handler(webHost{func() {}}, nil)
	session := webSession{cookie: "magpie_web_19443", key: "fake-admin-0123456789", keep: webCookieAge, secure: true}
	h, err := remoteWebHandler("https://server.example:19443", session, resolver, page)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ConnContext = remotejoin.TrustedServeConnContext
	srv.Start()
	defer srv.Close()
	client := srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req := func(method, path, host string, cookie *http.Cookie, key string) *http.Response {
		t.Helper()
		r, e := http.NewRequest(method, srv.URL+path, strings.NewReader("{}"))
		if e != nil {
			t.Fatal(e)
		}
		r.Host = host
		r.Header.Set("X-Forwarded-For", "100.75.211.11")
		r.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		resp, e := client.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	login := req("GET", remotejoin.BrowserLoginPath+"?back=/", "server.example:19443", nil, "")
	if login.StatusCode != 303 || len(login.Cookies()) != 1 {
		t.Fatal("trusted browser could not log in")
	}
	cookie := login.Cookies()[0]
	for _, method := range []string{"GET", "POST"} {
		if r := req(method, "/api/settings", "server.example:19443", cookie, ""); r.StatusCode != 200 {
			t.Fatalf("management %s got %d", method, r.StatusCode)
		}
	}
	if r := req("GET", "/api/settings", "server.example:19443", nil, gatewayKey); r.StatusCode != 401 {
		t.Fatalf("gateway key reached management: %d", r.StatusCode)
	}
	if r := req("GET", "/v1/models", "server.example:19443", cookie, ""); r.StatusCode != 401 {
		t.Fatalf("web cookie reached gateway: %d", r.StatusCode)
	}
	if r := req("GET", "/v1/models", "server.example:19443", nil, gatewayKey); r.StatusCode != 200 {
		t.Fatalf("gateway key failed: %d", r.StatusCode)
	}
	for _, target := range []string{remotejoin.BrowserLoginPath + "?back=//evil.example", remotejoin.BrowserLoginPath + "?back=%2f%2fevil.example", remotejoin.BrowserLoginPath + "?back=/&back=/other"} {
		if r := req("GET", target, "server.example:19443", nil, ""); r.StatusCode != 400 {
			t.Fatal("unsafe return path accepted")
		}
	}
	if r := req("GET", remotejoin.BrowserLoginPath, "evil.example", nil, ""); r.StatusCode != 400 || len(r.Cookies()) != 0 {
		t.Fatal("injected host accepted")
	}
	if again := req("GET", remotejoin.BrowserLoginPath, "server.example:19443", nil, ""); again.StatusCode != 303 || len(again.Cookies()) != 1 || again.Cookies()[0].Value != cookie.Value {
		t.Fatal("repeat login failed")
	}
	rotated := session
	rotated.key = "different-web-key-0123456789"
	rotatedHandler, err := remoteWebHandler("https://server.example:19443", rotated, resolver, page)
	if err != nil {
		t.Fatal(err)
	}
	rotatedServer := httptest.NewUnstartedServer(rotatedHandler)
	rotatedServer.Config.ConnContext = remotejoin.TrustedServeConnContext
	rotatedServer.Start()
	defer rotatedServer.Close()
	oldCookie, _ := http.NewRequest("GET", rotatedServer.URL+"/api/settings", nil)
	oldCookie.Host = "server.example:19443"
	oldCookie.AddCookie(cookie)
	result, err := rotatedServer.Client().Do(oldCookie)
	if err != nil {
		t.Fatal(err)
	}
	result.Body.Close()
	if result.StatusCode != 401 {
		t.Fatal("old cookie survived key rotation")
	}
	newLogin, _ := http.NewRequest("GET", rotatedServer.URL+remotejoin.BrowserLoginPath, nil)
	newLogin.Host = "server.example:19443"
	newLogin.Header.Set("X-Forwarded-For", "100.75.211.11")
	rotatedClient := rotatedServer.Client()
	rotatedClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	result, err = rotatedClient.Do(newLogin)
	if err != nil {
		t.Fatal(err)
	}
	result.Body.Close()
	if result.StatusCode != 303 || len(result.Cookies()) != 1 || result.Cookies()[0].Value != rotated.key {
		t.Fatal("login after key rotation failed")
	}
	tcp := httptest.NewServer(webServed("19443", session.key, session.keep, page))
	defer tcp.Close()
	r, _ := http.NewRequest("GET", tcp.URL+remotejoin.BrowserLoginPath, nil)
	r.Header.Set("X-Forwarded-For", "100.75.211.11")
	response, e := tcp.Client().Do(r)
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	if response.StatusCode == 303 || len(response.Cookies()) != 0 {
		t.Fatal("ordinary TCP installed login bridge")
	}
}
