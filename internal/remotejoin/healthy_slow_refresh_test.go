package remotejoin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHealthySlowRefreshCompletes(t *testing.T) {
	isolateStore(t)
	var root string
	var slow atomic.Bool
	var attempted, completed atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slow.Load() {
			attempted.Add(1)
			select {
			case <-time.After(700 * time.Millisecond):
				completed.Add(1)
			case <-r.Context().Done():
				return
			}
		}
		_ = json.NewEncoder(w).Encode(Registration{ProtocolVersion: 1, ServerVersion: "test", Device: RegisteredDevice{ID: "0123456789abcdef01234567", Peer: Peer{StableNodeID: "node", NodeID: "1", DisplayName: "node"}}, Gateway: GatewayConnection{BaseURL: root, KeyID: "0123456789abcdef01234567", Key: "sk-magpie-key-333333333333333333333333333333333333333333333333"}, Management: ManagementConnection{BaseURL: root, LoginURL: root + BrowserLoginPath}})
	}))
	defer srv.Close()
	root = srv.URL
	c := NewClient(ClientOptions{HTTPClient: srv.Client(), Binding: &fakeBinding{}})
	m := &runnerManaged{}
	r := NewRunner(c, RunnerOptions{Managed: func() (ManagedController, error) { return m, nil }, Policy: RunnerPolicy{ProbeEvery: 200 * time.Millisecond, RegistrationEvery: time.Hour, CatalogEvery: time.Hour}, Probe: func(context.Context, ClientStatus) HealthResult { return HealthResult{Healthy: true, Reachable: true} }})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-r.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("not ready")
	}
	if _, e := r.Connect(context.Background(), root); e != nil {
		t.Fatal("connect", e)
	}
	// Let the post-connect immediate registration and first tick finish.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s, _ := r.Status(context.Background())
		if s.Connected {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s, _ := r.Status(context.Background())
	if !s.Connected {
		t.Fatal("not connected")
	}
	slow.Store(true)
	for i := 0; i < 3; i++ {
		time.Sleep(50 * time.Millisecond)
		start := time.Now()
		_, e := r.Refresh(context.Background())
		if e != nil {
			t.Fatalf("attempt %d: refresh failed: %v", i+1, e)
		}
		t.Logf("healthy refresh %d completed in %v", i+1, time.Since(start))
	}
	if attempted.Load() != 3 || completed.Load() != 3 {
		t.Fatalf("attempted=%d completed=%d", attempted.Load(), completed.Load())
	}
	s, _ = r.Status(context.Background())
	if !s.Connected {
		t.Fatalf("health unexpectedly failed: %s", s.State)
	}
	t.Logf("normal server responses (700ms) 3/3 completed while health remained connected; 200ms probe interval")
}
