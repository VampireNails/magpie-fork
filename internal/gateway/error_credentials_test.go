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

func TestPolicyRefusalStreamDoesNotExposeProviderCredential(t *testing.T) {
	fresh(t)
	const key = "synthetic-policy-secret-9304"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("data: {\"error\":{\"code\":\"bio_policy\",\"message\":\"filtered " + key + "\"}}\n\n"))
	}))
	defer up.Close()
	if err := provider.Save(provider.Provider{ID: "fixture", Name: "Fixture", Key: key, Chat: up.URL + "/v1", Models: []string{"test"}}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"fixture/test","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	New().Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "hello") || strings.Contains(rec.Body.String(), key) {
		t.Fatal("policy error failed to preserve content or exposed the provider credential")
	}
}

func TestCountTokensErrorDoesNotExposeProviderCredential(t *testing.T) {
	fresh(t)
	const key = "synthetic-count-provider-key-3911"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", key)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid credential ` + key + `"}}`))
	}))
	defer up.Close()
	if err := provider.Save(provider.Provider{ID: "fixture", Name: "Fixture", Key: key, Anthropic: up.URL, Models: []string{"test"}}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(`{"model":"fixture/test","messages":[{"role":"user","content":"hello"}]}`))
	New().Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), key) || strings.Contains(rec.Header().Get("Request-Id"), key) {
		t.Fatalf("count_tokens leaked a provider credential or returned wrong status: status=%d, leaked=%t", rec.Code, strings.Contains(rec.Body.String(), key))
	}
}

func TestRefusalDetailDoesNotExposeCredential(t *testing.T) {
	const key = "synthetic-refusal-secret-1947"
	input := "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\",\"stop_details\":{\"message\":\"" + key + "\"}}}\n\n"
	reader := &upstreamErrorStream{body: io.NopCloser(strings.NewReader(input)), reader: bufio.NewReader(strings.NewReader(input)), secrets: []string{key}}
	out, err := io.ReadAll(reader)
	if err != nil || strings.Contains(string(out), key) || !strings.Contains(string(out), "[redacted]") {
		t.Fatal("refusal detail exposed the credential")
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
	input := []byte(`{"error":{"message":"key is synthetic-long-secret","code":"api_error"},"count":12345678901234567890}`)
	got := hideFailureBody(input, []string{"synthetic-long-secret"})
	if !json.Valid(got) || !strings.Contains(string(got), `"message"`) || !strings.Contains(string(got), `12345678901234567890`) || !strings.Contains(string(got), "[redacted]") {
		t.Fatalf("JSON error structure was not preserved: %s", got)
	}
}

func TestShortCredentialRedactsWholeTokensOnly(t *testing.T) {
	cases := map[string]string{
		"invalid key k":     "invalid key [redacted]",
		"key=k, retry":      "key=[redacted], retry",
		"k":                 "[redacted]",
		"prefixksuffix":     "prefixksuffix",
		"magpie-grok: down": "magpie-grok: down",
	}
	for in, want := range cases {
		if got := hideFailureSecrets(in, []string{"k"}); got != want {
			t.Fatalf("short credential redaction of %q = %q, want %q", in, got, want)
		}
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

func TestFailureJSONNumericCredential(t *testing.T) {
	const secret = "123456789"
	got := hideFailureBody([]byte(`{"error":{"code":123456789,"message":"invalid key"}}`), []string{secret})
	if bytes.Contains(got, []byte(secret)) || !json.Valid(got) {
		t.Fatal("numeric credential exposed or JSON invalid")
	}
}

func TestFailureEmbeddedShortCredential(t *testing.T) {
	const secret = "abc123"
	got := hideFailureBody([]byte(`{"error":{"message":"invalid credential: prefixabc123suffix"}}`), []string{secret})
	if bytes.Contains(got, []byte(secret)) {
		t.Fatal("embedded short credential exposed")
	}
}

func TestFailureCredentialNotReintroducedByMarker(t *testing.T) {
	got := hideFailureBody([]byte(`{"error":{"message":"invalid credential: ed"}}`), []string{"ed"})
	if bytes.Contains(got, []byte("ed")) {
		t.Fatal("marker reintroduced credential")
	}
}

func TestFailureJSONEncodedCredentialAsKey(t *testing.T) {
	const secret = "synthetic/key-7812"
	const encoded = "synthetic%2Fkey-7812"
	got := hideFailureBody([]byte(`{"error":{"synthetic%2Fkey-7812":"failed"}}`), []string{secret})
	if bytes.Contains(got, []byte(encoded)) || !json.Valid(got) {
		t.Fatal("encoded credential exposed in JSON property name")
	}
}

func TestFailureStreamMetadataCredential(t *testing.T) {
	const secret = "synthetic-stream-metadata-secret-5832"
	input := "id: " + secret + "\n: " + secret + "\ndata: {\"error\":{\"message\":\"invalid key\"}}\n\n"
	reader := &upstreamErrorStream{body: io.NopCloser(strings.NewReader(input)), reader: bufio.NewReader(strings.NewReader(input)), secrets: []string{secret}}
	out, err := io.ReadAll(reader)
	if err != nil || strings.Contains(string(out), secret) {
		t.Fatal("stream error metadata exposed credential")
	}
}

func TestFailureStreamPlainErrorCredential(t *testing.T) {
	const secret = "synthetic-plain-stream-secret-7482"
	input := "event: error\ndata: invalid credential " + secret + "\n\n"
	reader := &upstreamErrorStream{body: io.NopCloser(strings.NewReader(input)), reader: bufio.NewReader(strings.NewReader(input)), secrets: []string{secret}}
	out, err := io.ReadAll(reader)
	if err != nil || strings.Contains(string(out), secret) {
		t.Fatal("plain stream error exposed credential")
	}
}

func TestGatewayFailureJSONNumericCredential(t *testing.T) {
	fresh(t)
	const secret = "123456789"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":123456789,"message":"invalid credential"}}`)
	}))
	defer up.Close()
	if err := provider.Save(provider.Provider{ID: "fixture", Name: "Fixture", Key: secret, Chat: up.URL + "/v1", Models: []string{"test"}}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"fixture/test","messages":[{"role":"user","content":"hello"}]}`))
	New().Handler().ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("gateway error exposed numeric provider credential")
	}
}

func TestGatewayFailureEmbeddedShortCredential(t *testing.T) {
	fresh(t)
	const secret = "abc123"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid credential prefixabc123suffix"}}`)
	}))
	defer up.Close()
	if err := provider.Save(provider.Provider{ID: "fixture", Name: "Fixture", Key: secret, Chat: up.URL + "/v1", Models: []string{"test"}}); err != nil {
		t.Fatal(err)
	}
	s := New()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"fixture/test","messages":[{"role":"user","content":"hello"}]}`))
	s.Handler().ServeHTTP(rec, req)
	trace, _ := json.Marshal(s.Trace(context.Background(), 0, 0))
	if strings.Contains(rec.Body.String(), secret) || strings.Contains(lastRecord(t).Error, secret) || bytes.Contains(trace, []byte(secret)) {
		t.Fatal("gateway error, usage, or trace exposed embedded provider credential")
	}
}

func TestGatewayFailureStreamMetadataCredential(t *testing.T) {
	fresh(t)
	const secret = "synthetic-metadata-secret-7291"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "id: "+secret+"\n: "+secret+"\ndata: {\"error\":{\"message\":\"bad key\"}}\n\n")
	}))
	defer up.Close()
	if err := provider.Save(provider.Provider{ID: "fixture", Name: "Fixture", Key: secret, Chat: up.URL + "/v1", Models: []string{"test"}}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"fixture/test","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	New().Handler().ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("gateway error stream exposed provider credential in metadata")
	}
}

func TestGatewayShortCredentialPreservesErrorProtocol(t *testing.T) {
	fresh(t)
	const secret = "or"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid credential or"}}`)
	}))
	defer up.Close()
	if err := provider.Save(provider.Provider{ID: "fixture", Name: "Fixture", Key: secret, Chat: up.URL + "/v1", Models: []string{"test"}}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"fixture/test","messages":[{"role":"user","content":"hello"}]}`))
	New().Handler().ServeHTTP(rec, req)
	var reply struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil || reply.Error.Type != "authentication_error" || reply.Error.Message == "" || strings.Contains(reply.Error.Message, secret) {
		t.Fatalf("gateway error protocol damaged or dynamic credential exposed: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGatewayLongSSECommentDoesNotExposeCredential(t *testing.T) {
	fresh(t)
	const secret = "synthetic-long-comment-secret-7938"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, ": "+strings.Repeat("x", 5000)+secret+"\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"world\"}}]}\n\n")
	}))
	defer up.Close()
	if err := provider.Save(provider.Provider{ID: "fixture", Name: "Fixture", Key: secret, Chat: up.URL + "/v1", Models: []string{"test"}}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"fixture/test","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	New().Handler().ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), secret) || !strings.Contains(rec.Body.String(), "world") {
		t.Fatal("long SSE comment exposed credential or interrupted normal content")
	}
}

func TestFailureStreamShortCredentialPreservesErrorEvent(t *testing.T) {
	const secret = "or"
	input := "event: error\ndata: {\"error\":{\"message\":\"invalid credential or\"}}\n\n"
	reader := &upstreamErrorStream{body: io.NopCloser(strings.NewReader(input)), reader: bufio.NewReader(strings.NewReader(input)), secrets: []string{secret}}
	out, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(out), "event: error\n") || !strings.Contains(string(out), `"error"`) || strings.Contains(string(out), "credential or") {
		t.Fatalf("SSE error protocol damaged or dynamic credential exposed: %s", out)
	}
}

func TestGatewayShortCredentialPreservesSSEErrorProtocol(t *testing.T) {
	fresh(t)
	const secret = "or"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "event: error\ndata: {\"error\":{\"message\":\"invalid credential or\"}}\n\n")
	}))
	defer up.Close()
	if err := provider.Save(provider.Provider{ID: "fixture", Name: "Fixture", Key: secret, Chat: up.URL + "/v1", Models: []string{"test"}}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"fixture/test","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	New().Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "event: error\n") || !strings.Contains(body, `"error"`) || strings.Contains(body, "credential or") {
		t.Fatalf("gateway SSE error protocol damaged or dynamic credential exposed: %s", body)
	}
}
