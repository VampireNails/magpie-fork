package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestAC10NativeErrorPaths(t *testing.T) {
	for _, tc := range []struct {
		name, path, body, first, fail string
		responses                     bool
	}{
		{"responses", "/v1/responses", `{"model":"fixture/test","input":"hello"}`, `event: response.output_text.delta\ndata: {"type":"response.output_text.delta","delta":"hello","output_index":0,"content_index":0}\n\n`, `event: response.failed\ndata: {"type":"response.failed","response":{"status":"failed","error":{"code":"api_error","message":"failed KEY"}}}\n\n`, true},
		{"anthropic", "/v1/messages", `{"model":"fixture/test","max_tokens":20,"messages":[{"role":"user","content":"hello"}]}`, `event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}\n\n`, `event: error\ndata: {"type":"error","error":{"type":"api_error","message":"failed KEY"}}\n\n`, false},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "_http_error", true: "_stream_error"}[stream], func(t *testing.T) {
				fresh(t)
				const key = "synthetic-native-provider-key-732984"
				var hits atomic.Int32
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					w.Header().Set("X-Request-Id", key)
					if !stream {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(401)
						_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid ` + key + `"}}`))
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte(strings.ReplaceAll(tc.first, `\n`, "\n")))
					w.(http.Flusher).Flush()
					_, _ = w.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(tc.fail, "KEY", key), `\n`, "\n")))
				}))
				defer up.Close()
				p := provider.Provider{ID: "fixture", Name: "Fixture", Key: key, Models: []string{"test"}}
				if tc.responses {
					p.Responses = up.URL + "/v1"
				} else {
					p.Anthropic = up.URL
				}
				if e := provider.Save(p); e != nil {
					t.Fatal(e)
				}
				body := tc.body
				if stream {
					body = strings.TrimSuffix(body, "}") + `,"stream":true}`
				}
				s := New()
				rec := httptest.NewRecorder()
				s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", tc.path, strings.NewReader(body)))
				if hits.Load() == 0 {
					t.Fatalf("upstream not reached: status=%d", rec.Code)
				}
				if stream && !strings.Contains(rec.Body.String(), "hello") {
					t.Fatalf("stream content absent: status=%d", rec.Code)
				}
				if stream && !strings.Contains(rec.Body.String(), "[redacted]") {
					t.Fatal("stream error was not delivered with its credential redacted")
				}
				if !stream && rec.Code != 401 {
					t.Fatalf("error status=%d", rec.Code)
				}
				trace, e := json.Marshal(s.Trace(context.Background(), 0, 0))
				if e != nil {
					t.Fatal(e)
				}
				entry := lastRecord(t)
				entryJSON, e := json.Marshal(entry)
				if e != nil {
					t.Fatal(e)
				}
				if strings.Contains(rec.Body.String(), key) || strings.Contains(rec.Header().Get("X-Request-Id"), key) || strings.Contains(string(trace), key) || strings.Contains(string(entryJSON), key) {
					t.Fatalf("credential leaked: body=%t header=%t trace=%t usage=%t", strings.Contains(rec.Body.String(), key), strings.Contains(rec.Header().Get("X-Request-Id"), key), strings.Contains(string(trace), key), strings.Contains(string(entryJSON), key))
				}
			})
		}
	}
}
