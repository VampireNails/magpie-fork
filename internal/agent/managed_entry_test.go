package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/provider"
)

func managedEntryHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "LOCALAPPDATA", "APPDATA"} {
		t.Setenv(key, home)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("HERMES_HOME", "")
	t.Setenv("WORKBUDDY_CONFIG_DIR", "")
	if err := provider.Save(provider.Provider{ID: "fixture", Name: "Fixture", Chat: "https://fixture.invalid/v1", Key: "synthetic", Models: []string{"one"}}); err != nil {
		t.Fatal(err)
	}
	return home
}

func managedEntryConfigured(t *testing.T) {
	t.Helper()
	p := filepath.Join(appdir.Config(), "remote-connection.json")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"schemaVersion":1,"serverUrl":"https://fixture.invalid"}`), 0600); err != nil {
		t.Fatal(err)
	}
}

func managedEntryBytes(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Another CLI process has no injection but must recognize the persisted client.
func TestManagedEntryConfiguredWithoutOwnerRefusesPublicWrites(t *testing.T) {
	for _, id := range []string{"workbuddy", "hermes"} {
		for _, action := range []string{"Apply", "Pick", "ConnectHow"} {
			t.Run(id+"/"+action, func(t *testing.T) {
				home := managedEntryHome(t)
				a := workbuddy(home)
				key, value := "provider", "magpie"
				if id == "hermes" {
					a = hermes(home)
					key, value = "model", "magpie/fixture/one"
				}
				if err := os.MkdirAll(filepath.Dir(a.Path), 0700); err != nil {
					t.Fatal(err)
				}
				original := []byte("[]\n")
				if id == "hermes" {
					original = []byte("model:\n  default: own\n  provider: own\n")
				}
				if err := os.WriteFile(a.Path, original, 0600); err != nil {
					t.Fatal(err)
				}
				managedEntryConfigured(t)
				var err error
				switch action {
				case "Apply":
					err = a.Apply(key, value)
				case "Pick":
					err = a.Pick(key, value)
				case "ConnectHow":
					_, err = a.ConnectHow()
				}
				if err == nil {
					t.Error("configured remote client accepted a new agent without a resident owner")
				}
				if !bytes.Equal(managedEntryBytes(t, a.Path), original) {
					t.Error("ownerless public entry changed agent configuration")
				}
			})
		}
	}
}

func TestManagedEntryDetachedCatalogDoesNotWriteRememberedAgents(t *testing.T) {
	for _, id := range []string{"workbuddy", "hermes"} {
		t.Run(id, func(t *testing.T) {
			home := managedEntryHome(t)
			a := workbuddy(home)
			key, value := "provider", "magpie"
			if id == "hermes" {
				a = hermes(home)
				key, value = "model", "magpie/fixture/one"
			}
			if err := a.Apply(key, value); err != nil {
				t.Fatal(err)
			}
			before := managedEntryBytes(t, a.Path)
			managedEntryConfigured(t)
			if err := provider.Save(provider.Provider{ID: "fixture", Name: "Fixture", Chat: "https://fixture.invalid/v1", Key: "synthetic", Models: []string{"one", "two"}}); err != nil {
				t.Fatal(err)
			}
			SyncCatalog()
			if !bytes.Equal(managedEntryBytes(t, a.Path), before) {
				t.Error("detached catalog sync rewrote remembered agent configuration")
			}
		})
	}
}

func TestManagedEntryDetachedRewireDoesNotCallNativeWriters(t *testing.T) {
	managedEntryHome(t)
	managedEntryConfigured(t)
	for _, id := range []string{"workbuddy", "hermes"} {
		t.Run(id, func(t *testing.T) {
			writes := 0
			a := &Agent{ID: id, Name: id, Join: func() (bool, error) { writes++; return true, nil }, Joined: func() bool { return true }, Sync: func() error { writes++; return nil }}
			setAgain([]*Agent{a})
			if writes != 0 {
				t.Errorf("detached rewire invoked %d native writers", writes)
			}
		})
	}
}

func TestManagedEntryOwnerRestoresWorkBuddyAndDoesNotReconnect(t *testing.T) {
	home := managedEntryHome(t)
	managedEntryConfigured(t)
	a := workbuddy(home)
	if err := os.MkdirAll(filepath.Dir(a.Path), 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte("[{\"id\":\"local\",\"local\":true}]\n")
	if err := os.WriteFile(a.Path, original, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := NewManagedStore("")
	if err != nil {
		t.Fatal(err)
	}
	m, err := EnableManagedEntries(store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { managedOwners.Delete(appdir.Config()) })
	if err := m.Recover(); err != nil {
		t.Fatal(err)
	}
	m.SetConnection(true, true)
	if err := a.Apply("provider", "magpie"); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(managedEntryBytes(t, a.Path), original) {
		t.Fatal("connect did not write models")
	}
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(managedEntryBytes(t, a.Path), original) {
		t.Error("restore did not return B0")
	}
	m.SetConnection(true, true)
	SyncCatalog()
	if !bytes.Equal(managedEntryBytes(t, a.Path), original) {
		t.Error("service recovery rewrote detached config")
	}
}

func TestManagedEntryOwnerRestoresHermesAndRetriesConnection(t *testing.T) {
	home := managedEntryHome(t)
	managedEntryConfigured(t)
	a := hermes(home)
	if err := os.MkdirAll(filepath.Dir(a.Path), 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte("model:\n  default: own\n  provider: own\n")
	if err := os.WriteFile(a.Path, original, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := NewManagedStore("")
	if err != nil {
		t.Fatal(err)
	}
	m, err := EnableManagedEntries(store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { managedOwners.Delete(appdir.Config()) })
	if err := m.Recover(); err != nil {
		t.Fatal(err)
	}
	m.SetConnection(true, true)
	if err := a.Apply("model", "magpie/fixture/one"); err != nil {
		t.Fatal(err)
	}
	if err := a.Disconnect(); err != nil {
		t.Fatal(err)
	}
	b := managedEntryBytes(t, a.Path)
	if !bytes.Contains(b, []byte("default: own")) || !bytes.Contains(b, []byte("provider: own")) || bytes.Contains(b, []byte("magpie:")) {
		t.Error("Hermes baseline not restored")
	}
	if err := a.Apply("model", "magpie/fixture/one"); err != nil {
		t.Fatal(err)
	}
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
}

func TestManagedEntryRestoreDrainsInFlightWrite(t *testing.T) {
	home := managedEntryHome(t)
	managedEntryConfigured(t)
	a := workbuddy(home)
	if err := os.MkdirAll(filepath.Dir(a.Path), 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte("[]\n")
	if err := os.WriteFile(a.Path, original, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := NewManagedStore("")
	if err != nil {
		t.Fatal(err)
	}
	m, err := EnableManagedEntries(store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { managedOwners.Delete(appdir.Config()) })
	if err := m.Recover(); err != nil {
		t.Fatal(err)
	}
	m.SetConnection(true, true)
	if err := a.Apply("provider", "magpie"); err != nil {
		t.Fatal(err)
	}
	entered, release, written := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := managedEntryWrite(a, false, func(a *Agent) error { close(entered); <-release; return a.Sync() })
		written <- err
	}()
	<-entered
	restored := make(chan error, 1)
	go func() { restored <- m.Restore() }()
	close(release)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if err := <-restored; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(managedEntryBytes(t, a.Path), original) {
		t.Error("restore did not finish after the in-flight write")
	}
}

func TestManagedEntryCloseAllowsNewOwnerOnlyAfterRecovery(t *testing.T) {
	managedEntryHome(t)
	managedEntryConfigured(t)
	store, err := NewManagedStore("")
	if err != nil {
		t.Fatal(err)
	}
	m, err := EnableManagedEntries(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = EnableManagedEntries(store); err == nil {
		t.Fatal("second controller accepted")
	}
	if err = m.Recover(); err != nil {
		t.Fatal(err)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	n, err := EnableManagedEntries(store)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Close(); err == nil {
		t.Fatal("closed controller accepted again")
	}
	if err = n.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagedEntryActiveRenameUsesExistingLease(t *testing.T) {
	home := managedEntryHome(t)
	managedEntryConfigured(t)
	a := workbuddy(home)
	if err := os.MkdirAll(filepath.Dir(a.Path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.Path, []byte("[]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := NewManagedStore("")
	if err != nil {
		t.Fatal(err)
	}
	m, err := EnableManagedEntries(store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { managedOwners.Delete(appdir.Config()) })
	if err = m.Recover(); err != nil {
		t.Fatal(err)
	}
	m.SetConnection(true, true)
	if err = a.Apply("provider", "magpie"); err != nil {
		t.Fatal(err)
	}
	before, err := store.ManagedState("workbuddy")
	if err != nil {
		t.Fatal(err)
	}
	handled, err := managedEntryWrite(a, false, func(scoped *Agent) error { scoped.renameFoundGroups(); return nil })
	if !handled || err != nil {
		t.Fatalf("active rename: %v", err)
	}
	after, err := store.ManagedState("workbuddy")
	if err != nil {
		t.Fatal(err)
	}
	if before.Generation != after.Generation {
		t.Fatal("rename started a new generation")
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
}
