//go:build linux

package gui

import (
	"context"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/remotejoin"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRemoteServerLinuxSocketNativeHTTP(t *testing.T) {
	onePortHome(t)
	gatewayKey := onePortKey(t)
	prior := served.Swap(gateway.New())
	t.Cleanup(func() { served.Store(prior) })
	who := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"Node":{"StableID":"node-linux","ID":123,"ComputedName":"fixture"}}`)
	}))
	defer who.Close()
	c := &http.Client{Transport: bridgeTransport(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.URL.Scheme = "http"
		r.URL.Host = who.Listener.Addr().String()
		return who.Client().Transport.RoundTrip(r)
	})}
	session := webSession{cookie: "magpie_web_19443", key: "fake-admin-0123456789", keep: webCookieAge, secure: true}
	page := Handler(webHost{func() {}}, nil)
	h, err := remoteWebHandler("https://server.example:19443", session, remotejoin.NewLocalAPIResolver(c, time.Second), page)
	if err != nil {
		t.Fatal(err)
	}
	p := socketPath(t)
	ln, err := listenProtectedUnix(p)
	if err != nil {
		t.Fatal(err)
	}
	s := &http.Server{Handler: h, ConnContext: remotejoin.TrustedServeConnContext}
	done := make(chan struct{})
	go func() { defer close(done); s.Serve(ln) }()
	defer func() { s.Close(); <-done }()
	tr := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", p)
	}}
	defer tr.CloseIdleConnections()
	cli := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request := func(method, path string, cookie *http.Cookie, key string) *http.Response {
		t.Helper()
		r, _ := http.NewRequest(method, "http://server.example:19443"+path, strings.NewReader("{}"))
		r.Header.Set("X-Forwarded-For", "100.75.211.11")
		r.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		v, e := cli.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		io.Copy(io.Discard, v.Body)
		v.Body.Close()
		return v
	}
	login := request("GET", remotejoin.BrowserLoginPath, nil, "")
	if login.StatusCode != 303 || len(login.Cookies()) != 1 {
		t.Fatal("socket login failed")
	}
	ck := login.Cookies()[0]
	if registration := request("POST", remotejoin.RegistrationPath, nil, ""); registration.StatusCode != http.StatusOK {
		t.Fatalf("registration over protected socket returned %d", registration.StatusCode)
	}
	for _, method := range []string{"GET", "POST"} {
		if v := request(method, "/api/settings", ck, ""); v.StatusCode != 200 {
			t.Fatalf("settings %s returned %d", method, v.StatusCode)
		}
	}
	if v := request("GET", "/v1/models", ck, ""); v.StatusCode != 401 {
		t.Fatalf("cookie authenticated gateway: %d", v.StatusCode)
	}
	if v := request("GET", "/v1/models", nil, gatewayKey); v.StatusCode != 200 {
		t.Fatalf("gateway key failed: %d", v.StatusCode)
	}
	if v := request("GET", "/api/settings", nil, gatewayKey); v.StatusCode != 401 {
		t.Fatalf("gateway key authenticated management: %d", v.StatusCode)
	}
}

func TestRemoteServerLinuxGatewayBindsOnlyConfiguredLoopback(t *testing.T) {
	onePortHome(t)
	onePortKey(t) // enables native gateway sharing
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()
	t.Setenv("MAGPIE_ADDR", addr)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gateway.New().ListenAndServe(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(15 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway did not start on configured loopback")
		}
		time.Sleep(25 * time.Millisecond)
	}
	_, port, _ := net.SplitHostPort(addr)
	other, err := net.Listen("tcp", net.JoinHostPort("127.0.0.2", port))
	if err != nil {
		t.Fatal("gateway also bound other interfaces")
	}
	other.Close()
}
