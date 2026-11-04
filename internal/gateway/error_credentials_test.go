package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

func TestStreamingErrorAfterContentDoesNotExposeProviderCredential(t *testing.T) {
	for _, contentType := range []string{"text/event-stream", "text/plain"} {
		t.Run(contentType, func(t *testing.T) { testStreamingErrorCredential(t, contentType) })
	}
}

func testStreamingErrorCredential(t *testing.T, contentType string) {
	fresh(t)
	const key = "synthetic-stream-vendor-secret-9876"
	const other = "synthetic-unrelated-vendor-secret-4567"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write([]byte("data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"invalid key: " + key + "\",\"" + other + "\":\"field\"}}\n\n"))
	}))
	defer up.Close()
	if err := provider.Save(provider.Provider{ID: "fixture", Name: "Fixture", Key: key, Chat: up.URL + "/v1", Models: []string{"test"}}); err != nil {
		t.Fatal(err)
	}
	if err := provider.Save(provider.Provider{ID: "other", Name: "Other", Key: other, Chat: up.URL + "/v1", Models: []string{"other"}}); err != nil {
		t.Fatal(err)
	}
	s := New()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"fixture/test","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "hello") {
		t.Fatal("stream never began")
	}
	if strings.Contains(rec.Body.String(), key) || strings.Contains(rec.Body.String(), other) {
		t.Fatal("streamed error exposed provider credential")
	}
	trace, _ := json.Marshal(s.Trace(context.Background(), 0, 0))
	if strings.Contains(string(trace), key) || strings.Contains(string(trace), other) || strings.Contains(lastRecord(t).Error, key) || strings.Contains(lastRecord(t).Error, other) {
		t.Fatal("diagnostic exposed provider credential")
	}
}

func TestFailureJSONKeyAndSplitStream(t *testing.T) {
	const secret = "synthetic-key-field-2468"
	body := []byte(`{"error":{"message":"failed","` + secret + `":"value"}}`)
	if got := hideFailureBody(body, []string{secret}); bytes.Contains(got, []byte(secret)) || !json.Valid(got) {
		t.Fatal("JSON key was not redacted")
	}
	input := "data: {\"error\":{\"message\":\"" + secret + "\"}}\n\n"
	reader := &upstreamErrorStream{body: io.NopCloser(strings.NewReader(input)), reader: bufio.NewReaderSize(strings.NewReader(input), 16), secrets: []string{secret}}
	out, err := io.ReadAll(reader)
	if err != nil || strings.Contains(string(out), secret) || !strings.Contains(string(out), "[redacted]") {
		t.Fatal("split stream error not redacted")
	}
}

func TestErrorStreamPassesStandaloneKeepalive(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	s := &upstreamErrorStream{body: r, reader: bufio.NewReader(r)}
	got := make(chan string, 1)
	go func() { b := make([]byte, 100); n, _ := s.Read(b); got <- string(b[:n]) }()
	go func() { _, _ = io.WriteString(w, ": keepalive\n") }()
	select {
	case line := <-got:
		if line != ": keepalive\n" {
			t.Fatalf("unexpected keepalive: %q", line)
		}
	case <-time.After(time.Second):
		t.Fatal("standalone keepalive was buffered")
	}
}

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
