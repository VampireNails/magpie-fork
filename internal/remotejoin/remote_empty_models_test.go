package remotejoin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/catalog"
)

func TestNativeRemoteRefreshEmptyCatalog(t *testing.T) {
	isolateStore(t)
	var root string
	body := `{"data":[{"id":"one"},{"id":"two"}]}`
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == RegistrationPath {
			_ = json.NewEncoder(w).Encode(Registration{ProtocolVersion: 1, ServerVersion: "test", Device: RegisteredDevice{ID: "0123456789abcdef01234567", Peer: Peer{StableNodeID: "node", NodeID: "1", DisplayName: "node"}}, Gateway: GatewayConnection{BaseURL: root, KeyID: "0123456789abcdef01234567", Key: "sk-magpie-key-333333333333333333333333333333333333333333333333"}, Management: ManagementConnection{BaseURL: root, LoginURL: root + BrowserLoginPath}})
			return
		}
		if body == "HTTPFAIL" {
			http.Error(w, "failure", 503)
			return
		}
		if req.URL.Query().Get("after_id") != "" {
			http.Error(w, "page failure", 503)
			return
		}
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	root = srv.URL
	old := http.DefaultClient
	http.DefaultClient = srv.Client()
	defer func() { http.DefaultClient = old }()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewClient(ClientOptions{HTTPClient: srv.Client(), Now: func() time.Time { return now }})
	changed := 0
	oldChanged := catalog.Changed
	catalog.Changed = func() { changed++ }
	defer func() { catalog.Changed = oldChanged }()
	status, err := c.Configure(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	before := status.LastSyncAt
	for _, bad := range []string{`{}`, `{"data":null}`, `{"data":{}}`, `{"data":[{}]}`, `{"data":[null]}`, `{"error":{"message":"bad"}}`, `{"data":[],"error":{}}`, `{"data":[],"has_more":true}`, `{"data":[{"id":"one"}],"has_more":true,"last_id":"one"}`, `{`, "HTTPFAIL"} {
		body = bad
		now = now.Add(time.Minute)
		failed, err := c.Refresh(context.Background())
		ms, _, ok := catalog.Live(status.ProviderID)
		if err == nil || failed.LastErrorCode != "model_refresh_failed" || !failed.LastSyncAt.Equal(before) || !ok || len(ms) != 2 {
			t.Fatalf("invalid %s changed prior state: err=%v count=%d ok=%v", bad, err, len(ms), ok)
		}
	}
	for _, tc := range []struct {
		body string
		want int
	}{{`{"data":[{"id":"one"}]}`, 1}, {`{"data":[],"models":[{"id":"bogus"}]}`, 0}} {
		body = tc.body
		now = now.Add(time.Minute)
		status, err = c.Refresh(context.Background())
		if err != nil {
			t.Fatalf("refresh %s: %v", body, err)
		}
		ms, _, ok := catalog.Live(status.ProviderID)
		if !ok || len(ms) != tc.want || !status.LastSyncAt.Equal(now) {
			t.Fatalf("refresh %s: count=%d ok=%v sync=%v", body, len(ms), ok, status.LastSyncAt)
		}
	}
	if changed != 0 {
		t.Fatalf("quiet refresh invoked agent callback %d times", changed)
	}
}
