package remotejoin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

func TestHealthRejectsWrongShapeAndNeverReturnsActiveContent(t *testing.T) {
	isolateStore(t)
	body := `{"object":"models","data":[]}`
	key := "sk-magpie-key-333333333333333333333333333333333333333333333333"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/magpie/concurrency" || r.Header.Get("x-api-key") != key {
			t.Error("health did not use native key/path")
		}
		w.Write([]byte(body))
	}))
	defer server.Close()
	if err := provider.BindRemote("remote-health", server.URL, key); err != nil {
		t.Fatal(err)
	}
	c := NewClient(ClientOptions{HTTPClient: server.Client()})
	result := c.probeHealth(context.Background(), ClientStatus{ServerURL: server.URL, ProviderID: "remote-health"})
	if result.Code != "health_response_invalid" || result.Healthy {
		t.Fatal("non-concurrency response counted as healthy")
	}
	body = `{"object":"concurrency","data":[],"requests":1,"waiting":0,"tools":0,"active":[{"prompt":"active-secret-canary"}]}`
	result = c.probeHealth(context.Background(), ClientStatus{ServerURL: server.URL, ProviderID: "remote-health"})
	if !result.Healthy || !result.Reachable {
		t.Fatal("native concurrency shape rejected")
	}
}

func TestHealth401RequestsReregistrationAndTimeoutIsBounded(t *testing.T) {
	isolateStore(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer server.Close()
	if err := provider.BindRemote("remote-health", server.URL, "fake-key"); err != nil {
		t.Fatal(err)
	}
	c := NewClient(ClientOptions{HTTPClient: server.Client()})
	s := ClientStatus{ServerURL: server.URL, ProviderID: "remote-health"}
	got := c.probeHealth(context.Background(), s)
	if !got.Unauthorized || !got.Reachable || got.Healthy {
		t.Fatal("401 was not classified separately")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	got = c.probeHealth(ctx, s)
	if got.Healthy || got.Code == "" {
		t.Fatal("expired context counted as healthy")
	}
}
