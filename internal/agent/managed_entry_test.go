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
