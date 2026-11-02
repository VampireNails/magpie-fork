package netproxy

import (
	"github.com/yetone/magpie/internal/tailnetdns"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestConfiguredRemoteRefusesRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("credential sent to redirect destination") }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer source.Close()
	u, _ := url.Parse(source.URL)
	tailnetdns.SetTarget(u.Host)
	defer tailnetdns.SetTarget("")
	client := &http.Client{Transport: Dispatch(&http.Transport{})}
	req, _ := http.NewRequest("GET", source.URL, nil)
	req.Header.Set("x-api-key", "secret-canary")
	if _, err := client.Do(req); err == nil {
		t.Fatal("redirect was accepted")
	}
}
