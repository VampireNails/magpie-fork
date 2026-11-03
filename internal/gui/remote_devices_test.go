package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/access"
)

func TestRemoteDevicesManagement(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	a, err := access.EnsureDevice(context.Background(), access.DeviceIdentity{StableNodeID: "stable-a", NodeID: "1", DisplayName: "Laptop"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := access.EnsureDevice(context.Background(), access.DeviceIdentity{StableNodeID: "stable-b", NodeID: "2", DisplayName: "Server"})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	remoteDeviceRoutes(mux)
	h := webServed("3430", "fake-web-key", 0, noFramedAPI(mux))
	do := func(method, target string, cookie bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:3430"+target, nil)
		r.RemoteAddr = "127.0.0.1:12345"
		if cookie {
			r.AddCookie(&http.Cookie{Name: "magpie_web_3430", Value: "fake-web-key"})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := do("GET", "/api/remote-devices", false); w.Code != 401 {
		t.Fatalf("missing cookie: %d", w.Code)
	}
	w := do("GET", "/api/remote-devices?limit=1", true)
	var page access.DevicePage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.NextCursor == nil {
		t.Fatalf("first page status %d", w.Code)
	}
	if strings.Contains(w.Body.String(), a.Secret) || strings.Contains(w.Body.String(), b.Secret) {
		t.Fatal("device list contains credential")
	}
	w = do("GET", "/api/remote-devices?limit=1&cursor="+*page.NextCursor, true)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.NextCursor != nil {
		t.Fatalf("second page status %d", w.Code)
	}
	for _, query := range []string{"?limit=0", "?limit=101", "?limit=1&limit=2", "?cursor=bad", "?other=x"} {
		if w := do("GET", "/api/remote-devices"+query, true); w.Code != 400 {
			t.Errorf("invalid query %q: %d", query, w.Code)
		}
	}
	if w := do("DELETE", "/api/remote-devices/"+a.Device.ID, true); w.Code != 204 {
		t.Fatalf("remove: %d", w.Code)
	}
	if _, ok := access.Authenticate(a.Secret); ok {
		t.Fatal("removed credential still accepted")
	}
	if _, ok := access.Authenticate(b.Secret); !ok {
		t.Fatal("other device credential removed")
	}
	if w := do("DELETE", "/api/remote-devices/"+a.Device.ID, true); w.Code != 204 {
		t.Fatalf("repeat remove: %d", w.Code)
	}
	again, err := access.EnsureDevice(context.Background(), access.DeviceIdentity{StableNodeID: "stable-a", NodeID: "1", DisplayName: "Laptop"})
	if err != nil || again.Device.ID == a.Device.ID || again.Secret == a.Secret {
		t.Fatal("registration did not create a new credential")
	}
	bad := []byte("bad-store-canary")
	if err := os.WriteFile(access.Path(), bad, 0600); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "DELETE"} {
		path := "/api/remote-devices"
		if method == "DELETE" {
			path += "/" + b.Device.ID
		}
		w := do(method, path, true)
		if w.Code != 503 || strings.Contains(w.Body.String(), string(bad)) {
			t.Errorf("corrupt store %s: %d", method, w.Code)
		}
	}
	if got, err := os.ReadFile(access.Path()); err != nil || string(got) != string(bad) {
		t.Fatal("corrupt store changed")
	}
}

func TestRemoteDevicesNativeWebCookieAndCrossSite(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	d, err := access.EnsureDevice(context.Background(), access.DeviceIdentity{StableNodeID: "stable-http", NodeID: "7", DisplayName: "HTTP device"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := access.EnsureDevice(context.Background(), access.DeviceIdentity{StableNodeID: "stable-other", NodeID: "8", DisplayName: "Other device"})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	remoteDeviceRoutes(mux)
	s := httptest.NewServer(webServed("3430", "fake-web-key", 0, noFramedAPI(mux)))
	defer s.Close()
	unauthenticated, err := http.Get(s.URL + "/api/remote-devices")
	if err != nil {
		t.Fatal(err)
	}
	unauthenticated.Body.Close()
	if unauthenticated.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no cookie: %d", unauthenticated.StatusCode)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	login, err := client.PostForm(s.URL+webSignInPath, url.Values{"key": {"fake-web-key"}, "back": {"/"}})
	if err != nil {
		t.Fatal(err)
	}
	login.Body.Close()
	res, err := client.Get(s.URL + "/api/remote-devices")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("cookie list: %d", res.StatusCode)
	}
	r, err := http.NewRequest("DELETE", s.URL+"/api/remote-devices/"+d.Device.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Origin", "null")
	res, err = client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("null origin: %d", res.StatusCode)
	}
	if _, ok := access.Authenticate(d.Secret); !ok {
		t.Fatal("cross-site request removed the device")
	}
	r, err = http.NewRequest("DELETE", s.URL+"/api/remote-devices/"+d.Device.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Origin", "https://unrelated.example")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.Header.Set("Sec-Fetch-Dest", "empty")
	res, err = client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site: %d", res.StatusCode)
	}
	if _, ok := access.Authenticate(d.Secret); !ok {
		t.Fatal("cross-site request removed the device")
	}
	r, err = http.NewRequest("DELETE", s.URL+"/api/remote-devices/"+d.Device.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err = client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("authorized remove: %d", res.StatusCode)
	}
	if _, ok := access.Authenticate(d.Secret); ok {
		t.Fatal("removed key still authenticates")
	}
	if _, ok := access.Authenticate(other.Secret); !ok {
		t.Fatal("other device key stopped authenticating")
	}
	again, err := access.EnsureDevice(context.Background(), access.DeviceIdentity{StableNodeID: "stable-http", NodeID: "7", DisplayName: "HTTP device"})
	if err != nil || again.Device.ID == d.Device.ID || again.Secret == d.Secret {
		t.Fatal("registration did not replace the removed key")
	}
}
