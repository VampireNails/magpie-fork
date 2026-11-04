package remotejoin

import (
	"os"
	"testing"
	"time"
)

func TestRunnerLosingLocalConnectionStateClosesAndRestores(t *testing.T) {
	r, m, _, _ := runnerFixture(t)
	if s := tickRunner(t, r); !s.Connected || !m.connected {
		t.Fatal("fixture did not connect")
	}
	if err := os.Remove(connectionPath()); err != nil {
		t.Fatal(err)
	}
	s := tickRunner(t, r)
	if s.State != "unconfigured" || s.Connected || m.connected || m.restored != 1 {
		t.Fatalf("lost connection state: state=%s connected=%v managed=%v restores=%d", s.State, s.Connected, m.connected, m.restored)
	}
	tickRunner(t, r)
	if m.restored != 1 {
		t.Fatalf("repeated recovery without new connection: %d", m.restored)
	}
}

func TestRunnerMixedFailuresShareOfflineWindow(t *testing.T) {
	for _, storageFirst := range []bool{false, true} {
		name := "network-then-storage"
		if storageFirst {
			name = "storage-then-network"
		}
		t.Run(name, func(t *testing.T) {
			r, m, now, health := runnerFixture(t)
			tickRunner(t, r)
			original, err := os.ReadFile(connectionPath())
			if err != nil {
				t.Fatal(err)
			}
			if storageFirst {
				if err := os.WriteFile(connectionPath(), []byte("invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				*health = HealthResult{Code: "health_unreachable"}
			}
			tickRunner(t, r)
			*now = now.Add(r.options.Policy.OfflineWindow - time.Second)
			if storageFirst {
				if err := os.WriteFile(connectionPath(), original, 0600); err != nil {
					t.Fatal(err)
				}
				*health = HealthResult{Code: "health_unreachable"}
			} else if err := os.WriteFile(connectionPath(), []byte("invalid"), 0600); err != nil {
				t.Fatal(err)
			}
			if s := tickRunner(t, r); !s.Connected || m.restored != 0 {
				t.Fatal("detached before window expired")
			}
			*now = now.Add(time.Second)
			if s := tickRunner(t, r); s.Connected || m.connected || m.restored != 1 {
				t.Fatalf("continuous failure exceeded window: state=%s connected=%v restores=%d", s.State, s.Connected, m.restored)
			}
		})
	}
}

func TestRunnerHealthyProbeResetsMixedFailureWindow(t *testing.T) {
	r, m, now, health := runnerFixture(t)
	tickRunner(t, r)
	*health = HealthResult{Code: "health_unreachable"}
	tickRunner(t, r)
	*now = now.Add(r.options.Policy.OfflineWindow - time.Second)
	*health = HealthResult{Healthy: true, Reachable: true}
	if s := tickRunner(t, r); !s.Connected || s.State != "connected" {
		t.Fatal("health did not recover")
	}
	*now = now.Add(time.Second)
	if err := os.WriteFile(connectionPath(), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if s := tickRunner(t, r); s.State != "retrying" || !s.Connected || m.restored != 0 {
		t.Fatal("healthy probe did not reset window")
	}
}

func TestRunnerLosingLocalConnectionStateRetriesFailedRestore(t *testing.T) {
	r, m, _, _ := runnerFixture(t)
	tickRunner(t, r)
	m.restoreFail = true
	if err := os.Remove(connectionPath()); err != nil {
		t.Fatal(err)
	}
	s := tickRunner(t, r)
	if s.State != "restore-failed" || s.Connected || m.connected {
		t.Fatalf("failed restore: state=%s connected=%v managed=%v", s.State, s.Connected, m.connected)
	}
	m.restoreFail = false
	s = tickRunner(t, r)
	if s.State != "unconfigured" || s.Connected || m.restored != 2 {
		t.Fatalf("retry: state=%s connected=%v restores=%d", s.State, s.Connected, m.restored)
	}
}

func TestRunnerCorruptConnectionStateClosesAndRestores(t *testing.T) {
	r, m, now, _ := runnerFixture(t)
	tickRunner(t, r)
	if err := os.WriteFile(connectionPath(), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	s := tickRunner(t, r)
	if s.State != "retrying" || !s.Connected || !m.connected || m.restored != 0 {
		t.Fatal("temporary storage error detached immediately")
	}
	*now = now.Add(r.options.Policy.OfflineWindow)
	s = tickRunner(t, r)
	if s.State != "error" || s.LastErrorCode != "connection_storage_unavailable" || s.Connected || m.connected || m.restored != 1 {
		t.Fatalf("corrupt connection state: state=%s connected=%v managed=%v restores=%d", s.State, s.Connected, m.connected, m.restored)
	}
	if got, err := os.ReadFile(connectionPath()); err != nil || string(got) != "invalid" {
		t.Fatal("damaged state was overwritten")
	}
}

func TestRunnerTransientConnectionReadFailureKeepsWiring(t *testing.T) {
	r, m, now, _ := runnerFixture(t)
	tickRunner(t, r)
	original, err := os.ReadFile(connectionPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(connectionPath(), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if s := tickRunner(t, r); s.State != "retrying" || !s.Connected || m.restored != 0 {
		t.Fatal("transient read failure detached")
	}
	*now = now.Add(r.options.Policy.OfflineWindow / 2)
	if err := os.WriteFile(connectionPath(), original, 0600); err != nil {
		t.Fatal(err)
	}
	if s := tickRunner(t, r); !s.Connected || s.State != "connected" || !m.connected || m.restored != 0 {
		t.Fatal("recovered storage did not retain wiring")
	}
	*now = now.Add(r.options.Policy.OfflineWindow)
	if err := os.WriteFile(connectionPath(), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if s := tickRunner(t, r); s.State != "retrying" || !s.Connected || m.restored != 0 {
		t.Fatal("a later transient error inherited the previous retry window")
	}
}
