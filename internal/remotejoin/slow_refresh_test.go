package remotejoin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSlowRefreshDoesNotBlockOfflineRestore(t *testing.T) {
	isolateStore(t)
	var root string
	var stall atomic.Bool
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stall.Load() {
			entered <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		_ = json.NewEncoder(w).Encode(Registration{ProtocolVersion: 1, ServerVersion: "test", Device: RegisteredDevice{ID: "0123456789abcdef01234567", Peer: Peer{StableNodeID: "node", NodeID: "1", DisplayName: "node"}}, Gateway: GatewayConnection{BaseURL: root, KeyID: "0123456789abcdef01234567", Key: "sk-magpie-key-333333333333333333333333333333333333333333333333"}, Management: ManagementConnection{BaseURL: root, LoginURL: root + BrowserLoginPath}})
	}))
	defer srv.Close()
	root = srv.URL
	c := NewClient(ClientOptions{HTTPClient: srv.Client(), Binding: &fakeBinding{}})
	m := &runnerManaged{}
	var healthy atomic.Bool
	var watchProbe atomic.Bool
	probeObserved := make(chan struct{}, 1)
	healthy.Store(true)
	runner := NewRunner(c, RunnerOptions{Managed: func() (ManagedController, error) { return m, nil }, Policy: RunnerPolicy{OfflineWindow: 200 * time.Millisecond, ProbeEvery: 30 * time.Millisecond}, Probe: func(context.Context, ClientStatus) HealthResult {
		if healthy.Load() {
			if watchProbe.Load() {
				select {
				case probeObserved <- struct{}{}:
				default:
				}
			}
			return HealthResult{Healthy: true, Reachable: true}
		}
		return HealthResult{Code: "health_unreachable"}
	}})
	if err := runner.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer runner.stop()
	runner.tick(context.Background())
	if _, err := runner.Connect(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	runner.renew = func(ctx context.Context) (ClientStatus, error) { return c.Status(ctx) }
	runner.catalog = func(ctx context.Context) (ClientStatus, error) { return c.Status(ctx) }
	runner.tick(context.Background())
	healthy.Store(false)
	runner.tick(context.Background())
	stall.Store(true)
	doneRefresh := make(chan error, 1)
	go func() { _, err := runner.Refresh(context.Background()); doneRefresh <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not reach server")
	}
	time.Sleep(250 * time.Millisecond)
	healthy.Store(true)
	watchProbe.Store(true)
	began := time.Now()
	doneTick := make(chan struct{})
	go func() { runner.tick(context.Background()); close(doneTick) }()
	select {
	case <-probeObserved:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("healthy probe did not run while refresh was stalled")
	}
	healthy.Store(false)
	watchProbe.Store(false)
	select {
	case <-doneTick:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("tick blocked behind stalled refresh")
	}
	close(release)
	select {
	case err := <-doneRefresh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("refresh error = %v, want cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not end")
	}
	select {
	case <-doneTick:
	case <-time.After(5 * time.Second):
		t.Fatal("tick did not resume")
	}
	t.Logf("restore at %v after refresh began; count=%d state=%s", time.Since(began), m.restored, runner.snapshot().State)
	if m.restored != 1 || runner.snapshot().State != "offline-detached" {
		t.Fatalf("restore=%d state=%s", m.restored, runner.snapshot().State)
	}
	stall.Store(false)
	healthy.Store(true)
	runner.tick(context.Background())
	if s := runner.snapshot(); !s.Connected || s.State != "connected" {
		t.Fatalf("recovery state = %s, connected = %v", s.State, s.Connected)
	}
	if _, err := runner.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh after recovery: %v", err)
	}
}
