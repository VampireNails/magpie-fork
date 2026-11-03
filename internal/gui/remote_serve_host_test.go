package gui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/remotejoin"
)

func TestRemoteServerServeUnixForwardedHost(t *testing.T) {
	onePortHome(t)
	who := &http.Client{Transport: bridgeTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"Node":{"StableID":"node","ID":42,"ComputedName":"test"}}`)), Request: r}, nil
	})}
	resolver := remotejoin.NewLocalAPIResolver(who, time.Second)
	for _, tc := range []struct{ origin, authority string }{{"https://server.example", "server.example"}, {"https://server.example:19453", "server.example:19453"}} {
		session := webSession{cookie: "test", key: "fake-web-key-0123456789", keep: webCookieAge, secure: true}
		h, err := remoteWebHandler(tc.origin, session, resolver, http.NotFoundHandler())
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{remotejoin.RegistrationPath, remotejoin.BrowserLoginPath + "?back=/"} {
			for _, x := range []struct {
				name    string
				host    []string
				origin  string
				trusted bool
				want    int
			}{
				{"serve", []string{tc.authority}, tc.origin, true, 200},
				{"foreign host", []string{"foreign.invalid"}, "", true, 400},
				{"duplicate host", []string{tc.authority, tc.authority}, "", true, 400},
				{"missing host", nil, "", true, 400},
				{"foreign origin", []string{tc.authority}, "https://foreign.invalid", true, 400},
				{"untrusted", []string{tc.authority}, "", false, 401},
			} {
				t.Run(tc.authority+path+x.name, func(t *testing.T) {
					method := "POST"
					want := x.want
					if strings.Contains(path, "browser-login") {
						method = "GET"
						if want == 200 {
							want = 303
						}
					}
					req := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader("{}"))
					req.Header["X-Forwarded-Host"] = x.host
					req.Header.Set("X-Forwarded-Proto", "https")
					req.Header.Set("X-Forwarded-For", "100.75.211.11")
					req.Header.Set("Content-Type", "application/json")
					if x.origin != "" {
						req.Header.Set("Origin", x.origin)
					}
					if x.trusted {
						req = req.WithContext(remotejoin.TrustedServeConnContext(req.Context(), nil))
					}
					w := httptest.NewRecorder()
					h.ServeHTTP(w, req)
					if w.Code != want {
						t.Fatalf("got %d want %d", w.Code, want)
					}
					if want == 303 && (w.Header().Get("Location") != "/" || len(w.Result().Cookies()) != 1) {
						t.Fatal("missing clean redirect or cookie")
					}
					if want >= 400 && len(w.Result().Cookies()) != 0 {
						t.Fatal("denial issued cookie")
					}
				})
			}
		}
	}
}

func TestRemoteServerServeUnixNativeHost(t *testing.T) {
	onePortHome(t)
	session := webSession{cookie: "test", key: "fake-web-key-0123456789", keep: webCookieAge, secure: true}
	h, err := remoteWebHandler("https://server.example:19453", session, testRemoteResolver{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Observed-Host", r.Host)
		w.Header().Set("Observed-Scheme", r.URL.Scheme)
		w.WriteHeader(204)
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		host string
		want int
	}{{"server.example:19453", 204}, {"foreign.invalid", 400}} {
		r := httptest.NewRequest("GET", "http://localhost/api/test", nil)
		r.Header.Set("X-Forwarded-Host", tc.host)
		r.Header.Set("X-Forwarded-Proto", "https")
		r.AddCookie(&http.Cookie{Name: session.cookie, Value: session.key})
		r = r.WithContext(remotejoin.TrustedServeConnContext(r.Context(), nil))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("host %q got %d want %d", tc.host, w.Code, tc.want)
		}
		if tc.want == 204 && (w.Header().Get("Observed-Host") != "server.example:19453" || w.Header().Get("Observed-Scheme") != "https") {
			t.Fatalf("native got host %q", w.Header().Get("Observed-Host"))
		}
	}
}
