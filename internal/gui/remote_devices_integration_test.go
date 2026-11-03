package gui

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/yetone/magpie/internal/access"
)

func TestRemoteDevicesProductionHandlerWebSecurity(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	d, err := access.EnsureDevice(context.Background(), access.DeviceIdentity{StableNodeID: "stable-integrated", NodeID: "19", DisplayName: "Node"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(webServed("3430", "fake-web-key-0123456789", 0, Handler(webHost{}, nil)))
	defer server.Close()
	res, err := http.Get(server.URL + "/api/remote-devices")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list: %d", res.StatusCode)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	res, err = client.PostForm(server.URL+webSignInPath, url.Values{"key": {"fake-web-key-0123456789"}, "back": {"/"}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	res, err = client.Get(server.URL + "/api/remote-devices")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("authenticated device list: %d", res.StatusCode)
	}
	for _, method := range []string{"GET", "DELETE"} {
		path := "/api/remote-devices"
		if method == "DELETE" {
			path += "/" + d.Device.ID
		}
		r, err := http.NewRequest(method, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Origin", "https://foreign.example")
		res, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("foreign Origin without Fetch Metadata %s: %d", method, res.StatusCode)
		}
	}
	if _, ok := access.Authenticate(d.Secret); !ok {
		t.Fatal("foreign request removed the key")
	}
	r, err := http.NewRequest("GET", server.URL+"/api/remote-devices", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Referer", "https://foreign.example/path")
	res, err = client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Referer: %d", res.StatusCode)
	}
	r, err = http.NewRequest("DELETE", server.URL+"/api/remote-devices/"+d.Device.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Origin", server.URL)
	res, err = client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("same-origin deletion: %d", res.StatusCode)
	}
	if _, ok := access.Authenticate(d.Secret); ok {
		t.Fatal("device key still works")
	}
	res, err = client.Get(server.URL + "/api/remote-connection")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("server exposed client control: %d", res.StatusCode)
	}
}
