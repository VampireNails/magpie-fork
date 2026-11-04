package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestUpstreamErrorDoesNotExposeProviderCredential(t *testing.T) {
	const key = "synthetic-vendor-api-secret-7291"
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			fresh(t)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Request-Id", key)
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"type":"invalid_api_key","message":"invalid key: ` + key + `"}}`))
			}))
			defer up.Close()
			if err := provider.Save(provider.Provider{ID: "fixture", Name: "Fixture", Key: key, Chat: up.URL + "/v1", Models: []string{"test"}}); err != nil {
				t.Fatal(err)
			}
			s := New()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", path, strings.NewReader(`{"model":"fixture/test","max_tokens":20,"messages":[{"role":"user","content":"hello"}]}`))
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), key) || !strings.Contains(rec.Body.String(), "[redacted]") || strings.Contains(rec.Header().Get("X-Request-Id"), key) {
				t.Fatalf("unsafe error reply: status %d, secret present %t", rec.Code, strings.Contains(rec.Body.String(), key))
			}
			entry := lastRecord(t)
			if strings.Contains(entry.Error, key) || strings.Contains(entry.RequestID, key) || !strings.Contains(entry.Error, "[redacted]") {
				t.Fatal("usage error exposes credential or loses diagnostic")
			}
			trace, err := json.Marshal(s.Trace(context.Background(), 0, 0))
			if err != nil || strings.Contains(string(trace), key) {
				t.Fatal("routing trace exposes credential")
			}
		})
	}
}

func TestHideFailureBodyPreservesJSONStructure(t *testing.T) {
	input := []byte(`{"error":{"message":"key is a","code":"api_error"},"count":12345678901234567890}`)
	got := hideFailureBody(input, []string{"a"})
	if !json.Valid(got) || !strings.Contains(string(got), `"message"`) || !strings.Contains(string(got), `12345678901234567890`) || !strings.Contains(string(got), "[redacted]") {
		t.Fatalf("JSON error structure was not preserved: %s", got)
	}
}

func TestLastAttemptHoldsErrorBeforeRelease(t *testing.T) {
	client := httptest.NewRecorder()
	h := newHoldWriter(client, false)
	h.WriteHeader(http.StatusUnauthorized)
	_, _ = h.Write([]byte("error body"))
	if h.passing || client.Body.Len() != 0 || h.held.String() != "error body" {
		t.Fatal("last attempt sent an error before credentials could be removed")
	}
}
