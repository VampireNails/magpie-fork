package access

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRestoreValidBackupRepairsCorruptStore(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "keys", true: "nil-is-empty-replacement"}[empty], func(t *testing.T) {
			storeFixture(t)
			keys, err := Export()
			if err != nil {
				t.Fatal(err)
			}
			if empty {
				keys = nil
			}
			broken := []byte(`{"canary-corrupt-store":`)
			if err := os.WriteFile(Path(), broken, 0600); err != nil {
				t.Fatal(err)
			}
			if err := Restore(keys); err != nil {
				t.Fatalf("valid backup could not repair corrupt store: %v", err)
			}
			got, err := Export()
			if err != nil || !reflect.DeepEqual(got, keys) {
				t.Fatal("restored keys differ from explicit backup")
			}
			kept, err := filepath.Glob(Path() + ".bad-*")
			if err != nil || len(kept) != 1 {
				t.Fatal("damaged original not preserved")
			}
			b, err := os.ReadFile(kept[0])
			if err != nil || !bytes.Equal(b, broken) {
				t.Fatal("damaged original copy differs")
			}
		})
	}
}

func TestRestoreInvalidBackupDoesNotWrite(t *testing.T) {
	storeFixture(t)
	before, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := Restore([]Key{{ID: "invalid", Limit: &Limit{Cost: math.NaN()}}}); err == nil {
		t.Error("invalid backup accepted")
	}
	after, err := os.ReadFile(Path())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("invalid backup changed other keys")
	}
}

func TestCorruptStoreOrdinaryChangesNeverOverwrite(t *testing.T) {
	for name := range storeActions(DeviceCredential{}, "") {
		if name == "restore" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			c, admin := storeFixture(t)
			broken := []byte("broken")
			if err := os.WriteFile(Path(), broken, 0600); err != nil {
				t.Fatal(err)
			}
			// With no legacy settings to migrate, migration is a no-op.
			if err := storeActions(c, admin)[name](); err == nil && name != "migrate" {
				t.Error("ordinary operation treated damaged store as empty")
			}
			after, err := os.ReadFile(Path())
			if err != nil || !bytes.Equal(after, broken) {
				t.Fatal("ordinary operation overwrote damaged store")
			}
		})
	}
}
