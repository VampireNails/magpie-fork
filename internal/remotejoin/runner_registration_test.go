package remotejoin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRunnerRegistrationRepairsKeyWhenCatalogFails(t *testing.T) {
	isolateStore(t)
	var root string
	key := "sk-magpie-key-333333333333333333333333333333333333333333333333"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(Registration{ProtocolVersion: 1, ServerVersion: "test", Device: RegisteredDevice{ID: "0123456789abcdef01234567", Peer: Peer{StableNodeID: "node", NodeID: "1", DisplayName: "node"}}, Gateway: GatewayConnection{BaseURL: root, KeyID: "0123456789abcdef01234567", Key: key}, Management: ManagementConnection{BaseURL: root, LoginURL: root + BrowserLoginPath}})
	}))
	defer server.Close()
	root = server.URL
	binding := &fakeBinding{}
	client := NewClient(ClientOptions{HTTPClient: server.Client(), Binding: binding})
	before, err := client.Configure(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	key = "sk-magpie-key-444444444444444444444444444444444444444444444444"
	binding.fail = true
	after, err := client.registerOnly(context.Background())
	if err != nil || binding.key != key {
		t.Fatal("catalog failure prevented successful key repair")
	}
	if !before.LastSyncAt.Equal(after.LastSyncAt) {
		t.Fatal("registration changed last successful catalog time")
	}
}

func TestRunnerCatalogRefreshDoesNotReregister(t *testing.T) {
	isolateStore(t)
	var root string
	registrations := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		registrations++
		json.NewEncoder(w).Encode(Registration{ProtocolVersion: 1, ServerVersion: "test", Device: RegisteredDevice{ID: "0123456789abcdef01234567", Peer: Peer{StableNodeID: "node", NodeID: "1", DisplayName: "node"}}, Gateway: GatewayConnection{BaseURL: root, KeyID: "0123456789abcdef01234567", Key: "sk-magpie-key-333333333333333333333333333333333333333333333333"}, Management: ManagementConnection{BaseURL: root, LoginURL: root + BrowserLoginPath}})
	}))
	defer server.Close()
	root = server.URL
	binding := &fakeBinding{}
	client := NewClient(ClientOptions{HTTPClient: server.Client(), Binding: binding})
	before, err := client.Configure(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	binding.fail = true
	after, err := client.refreshCatalog(context.Background())
	if err == nil || registrations != 1 || !before.LastSyncAt.Equal(after.LastSyncAt) {
		t.Fatal("failed catalog refresh reregistered or changed sync time")
	}
	binding.fail = false
	if _, err = client.refreshCatalog(context.Background()); err != nil || registrations != 1 {
		t.Fatal("catalog refresh unexpectedly registered")
	}
}

func TestRunnerManagementAvailableWhenHealthyAndCatalogFails(t *testing.T) {
	r, _, _, _ := runnerFixture(t)
	err := withConnection(func(f *connectionFile) (bool, error) {
		f.DeviceID = "0123456789abcdef01234567"
		f.ManagementURL = f.ServerURL + BrowserLoginPath
		f.State = "error"
		f.LastErrorCode = "model_refresh_failed"
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r.catalog = func(context.Context) (ClientStatus, error) { return ClientStatus{}, errors.New("test") }
	tickRunner(t, r)
	u, err := r.ManagementURL(context.Background())
	if err != nil || u != "https://node.test"+BrowserLoginPath {
		t.Fatal("healthy server lost management due to catalog error")
	}
}

func TestRunnerConnectAndRefreshPublishValidatedCurrentStatus(t *testing.T) {
	isolateStore(t)
	var root string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(Registration{ProtocolVersion: 1, ServerVersion: "test", Device: RegisteredDevice{ID: "0123456789abcdef01234567", Peer: Peer{StableNodeID: "node", NodeID: "1", DisplayName: "node"}}, Gateway: GatewayConnection{BaseURL: root, KeyID: "0123456789abcdef01234567", Key: "sk-magpie-key-333333333333333333333333333333333333333333333333"}, Management: ManagementConnection{BaseURL: root, LoginURL: root + BrowserLoginPath}})
	}))
	defer server.Close()
	root = server.URL
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	client := NewClient(ClientOptions{HTTPClient: server.Client(), Binding: &fakeBinding{}, Now: func() time.Time { return now }})
	m := &runnerManaged{}
	r := NewRunner(client, RunnerOptions{Managed: func() (ManagedController, error) { return m, nil }, Now: func() time.Time { return now }, Probe: func(context.Context, ClientStatus) HealthResult { return HealthResult{Healthy: true, Reachable: true} }})
	if err := r.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.stop() })
	r.tick(context.Background())
	s, err := r.Connect(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if s.ServerURL != root || s.LastSyncAt.IsZero() || s.Connected {
		t.Fatal("connect returned stale or falsely live status")
	}
	current, _ := r.Status(context.Background())
	if current.ServerURL != root || !current.LastSyncAt.Equal(s.LastSyncAt) {
		t.Fatal("connect did not publish status")
	}
	now = now.Add(time.Minute)
	s, err = r.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	current, _ = r.Status(context.Background())
	if !s.LastSyncAt.Equal(now) || !current.LastSyncAt.Equal(now) {
		t.Fatal("refresh returned stale sync time")
	}
}

func TestRunnerRefreshFailurePublishesActualPhase(t *testing.T) {
	isolateStore(t)
	var root string
	reject := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if reject {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(Registration{ProtocolVersion: 1, ServerVersion: "test", Device: RegisteredDevice{ID: "0123456789abcdef01234567", Peer: Peer{StableNodeID: "node", NodeID: "1", DisplayName: "node"}}, Gateway: GatewayConnection{BaseURL: root, KeyID: "0123456789abcdef01234567", Key: "sk-magpie-key-333333333333333333333333333333333333333333333333"}, Management: ManagementConnection{BaseURL: root, LoginURL: root + BrowserLoginPath}})
	}))
	defer server.Close()
	root = server.URL
	binding := &fakeBinding{}
	client := NewClient(ClientOptions{HTTPClient: server.Client(), Binding: binding})
	m := &runnerManaged{}
	r := NewRunner(client, RunnerOptions{Managed: func() (ManagedController, error) { return m, nil }, Probe: func(context.Context, ClientStatus) HealthResult { return HealthResult{Healthy: true, Reachable: true} }})
	if err := r.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.stop() })
	r.tick(context.Background())
	if _, err := r.Connect(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	r.tick(context.Background())
	before, _ := r.Status(context.Background())
	binding.fail = true
	s, err := r.Refresh(context.Background())
	if err == nil || s.LastErrorCode != "model_refresh_failed" || !s.Connected || !s.LastSyncAt.Equal(before.LastSyncAt) {
		t.Fatal("catalog failure lost healthy state or successful sync time")
	}
	reject = true
	s, err = r.Refresh(context.Background())
	if err == nil || s.LastErrorCode != "registration_rejected" || !s.Connected || s.ServerURL != root {
		t.Fatal("registration failure not classified")
	}
}
