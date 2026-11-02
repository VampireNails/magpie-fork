package gui

import (
	"context"
	"github.com/yetone/magpie/internal/remotejoin"
	"github.com/yetone/magpie/internal/settings"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type testRemoteResolver struct{}

func (testRemoteResolver) Resolve(ctx context.Context, r *http.Request) (remotejoin.Peer, error) {
	return remotejoin.Peer{StableNodeID: "node", NodeID: "42", DisplayName: "fixture"}, nil
}

func TestRemoteServerLoginIssuesNativeCookieAndKeepsRoutes(t *testing.T) {
	session := webSession{cookie: "magpie_web_19443", key: "fake-key-0123456789", keep: webCookieAge, secure: true}
	h, err := remoteWebHandler("https://server.example:19443", session, testRemoteResolver{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "https://server.example:19443/_magpie/remote/v1/browser-login?back=/", nil)
	r = r.WithContext(remotejoin.TrustedServeConnContext(r.Context(), nil))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	cs := w.Result().Cookies()
	if w.Code != 303 || w.Header().Get("Location") != "/" || len(cs) != 1 || !cs[0].Secure || !cs[0].HttpOnly || cs[0].Name != session.cookie || cs[0].MaxAge != int((400*24*time.Hour).Seconds()) {
		t.Fatal("verified login did not issue native secure cookie and redirect")
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("login headers missing")
	}
}
func TestRemoteServerRejectsUnboundGatewayBeforeListener(t *testing.T) {
	onePortHome(t)
	t.Setenv("MAGPIE_WEB_KEY", "fake-web-key-0123456789")
	s := settings.Load()
	s.LAN = true
	s.GatewayMode = "off"
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{"", "0.0.0.0:14325", ":14325", "192.0.2.2:14325", "localhost:14325", "127.0.0.1:0"} {
		t.Setenv("MAGPIE_ADDR", addr)
		_, err := StartRemoteWeb("relative/socket", "https://server.example:19443", "test")
		if err == nil || !strings.Contains(err.Error(), "MAGPIE_ADDR") {
			t.Fatalf("remote mode accepted unsafe gateway address %q", addr)
		}
	}
}
