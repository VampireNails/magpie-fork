package backup

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/settings"
)

func TestGatewayBackupRepairsDamagedKeys(t *testing.T) {
	home(t)
	if err := access.ConfigureLAN(true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := access.EnsureDevice(context.Background(), access.DeviceIdentity{"backup-node", "1", "backup device"}); err != nil {
		t.Fatal(err)
	}
	b, err := Collect(true, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := Seal(b, "fixture passphrase")
	if err != nil {
		t.Fatal(err)
	}
	b, err = Open(sealed, "fixture passphrase")
	if err != nil {
		t.Fatal(err)
	}
	// Explicit restore replaces, rather than merges, all current keys.
	if _, err := access.EnsureDevice(context.Background(), access.DeviceIdentity{"other-node", "2", "other device"}); err != nil {
		t.Fatal(err)
	}
	broken := []byte(`{"canary-damaged-current":`)
	if err := os.WriteFile(access.Path(), broken, 0600); err != nil {
		t.Fatal(err)
	}
	r, err := Restore(b, Parts{Settings: true})
	if err != nil || !r.Settings {
		t.Fatalf("valid sealed backup cannot repair damaged keys: %v", err)
	}
	got, err := access.Export()
	if err != nil || !reflect.DeepEqual(got, *b.GatewayKeys) {
		t.Fatal("explicit backup did not replace the key store")
	}
	kept, err := filepath.Glob(access.Path() + ".bad-*")
	if err != nil || len(kept) != 1 {
		t.Fatal("damaged keys were not preserved")
	}
	old, err := os.ReadFile(kept[0])
	if err != nil || !bytes.Equal(old, broken) {
		t.Fatal("preserved damaged store differs")
	}
}

func TestGatewayBackupFailureRollsSettingsBack(t *testing.T) {
	for _, failure := range []string{"lock-unavailable", "invalid-keys", "unreadable-store"} {
		t.Run(failure, func(t *testing.T) {
			home(t)
			if err := access.ConfigureLAN(true, false); err != nil {
				t.Fatal(err)
			}
			cur := settings.Load()
			before, err := os.ReadFile(access.Path())
			if err != nil {
				t.Fatal(err)
			}
			incoming := cur
			incoming.Theme, incoming.LANKeyID, incoming.LANKey = "dark", "incoming", "sk-magpie-fixture-incoming"
			keys := []access.Key{{ID: incoming.LANKeyID, Secret: incoming.LANKey, LAN: true}}
			switch failure {
			case "lock-unavailable":
				if err := os.Remove(access.Path() + ".lock"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(access.Path()+".lock", 0700); err != nil {
					t.Fatal(err)
				}
			case "invalid-keys":
				keys[0].Limit = &access.Limit{Cost: math.NaN()}
			case "unreadable-store":
				if err := os.Remove(access.Path()); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(access.Path(), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(access.Path(), "kept"), before, 0600); err != nil {
					t.Fatal(err)
				}
			}
			r, err := Restore(Bundle{Version: 1, Keys: true, Settings: &incoming, GatewayKeys: &keys}, Parts{Settings: true})
			if err == nil || r.Settings {
				t.Error("failed backup restore reported success")
			}
			if !reflect.DeepEqual(settings.Load(), cur) {
				t.Error("failed backup restore did not roll settings back")
			}
			path := access.Path()
			if failure == "unreadable-store" {
				path = filepath.Join(path, "kept")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Error("failed backup restore changed other keys")
			}
		})
	}
}
