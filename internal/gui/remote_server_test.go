package gui

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRemoteServerNativeHTTPSCookieIsSecure(t *testing.T) {
	h := webGuard("magpie_web_19443", "test-web-key-0123456789", webCookieAge, http.NotFoundHandler())
	r := httptest.NewRequest("GET", "https://node.example:19443/?k=test-web-key-0123456789", nil)
	r.TLS = &tls.ConnectionState{}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	cs := w.Result().Cookies()
	if w.Code != http.StatusSeeOther || len(cs) != 1 {
		t.Fatal("native sign-in did not issue one cookie")
	}
	if !cs[0].Secure || !cs[0].HttpOnly || cs[0].SameSite != http.SameSiteLaxMode || cs[0].MaxAge != int(webCookieAge.Seconds()) {
		t.Fatal("HTTPS native sign-in cookie lacks the required browser attributes")
	}
}
