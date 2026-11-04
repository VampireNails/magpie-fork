package access

import (
	"context"
	"testing"
)

func TestDeviceIdentityRecoveryAndBackupRollback(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	a := DeviceIdentity{"synthetic-a", "101", "same-name"}
	b := DeviceIdentity{"synthetic-b", "202", "same-name"}
	first, err := EnsureDevice(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	other, err := EnsureDevice(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := Export()
	if err != nil {
		t.Fatal(err)
	}
	// A rename or changed node ID with the same stable identity retains A.
	a.DisplayName, a.NodeID = "new-name", "303"
	renamed, err := EnsureDevice(ctx, a)
	if err != nil || renamed.Device.ID != first.Device.ID || renamed.Secret != first.Secret {
		t.Fatal("same identity did not retain credential", err)
	}
	// A rebuilt node is a distinct identity even if its name matches B.
	rebuilt, err := EnsureDevice(ctx, DeviceIdentity{"synthetic-rebuilt", "404", "same-name"})
	if err != nil || rebuilt.Device.ID == other.Device.ID || rebuilt.Secret == other.Secret {
		t.Fatal("rebuilt node merged with B", err)
	}
	if err := RemoveDevice(ctx, first.Device.ID); err != nil {
		t.Fatal(err)
	}
	replacement, err := EnsureDevice(ctx, a)
	if err != nil || replacement.Secret == first.Secret {
		t.Fatal("re-registration did not rotate credential", err)
	}
	if _, ok := Authenticate(other.Secret); !ok {
		t.Fatal("B lost access after A removal")
	}
	// Restore is full replacement: an old backup revives the old credential.
	if err := Restore(backup); err != nil {
		t.Fatal(err)
	}
	if _, ok := Authenticate(first.Secret); !ok {
		t.Fatal("backup did not restore old A credential")
	}
	if _, ok := Authenticate(replacement.Secret); ok {
		t.Fatal("newer credential survived old backup")
	}
	if _, ok := Authenticate(other.Secret); !ok {
		t.Fatal("B lost access on restore")
	}
	recovered, err := EnsureDevice(ctx, a)
	if err != nil || recovered.Device.ID != first.Device.ID || recovered.Secret != first.Secret {
		t.Fatal("A failed to regain restored identity", err)
	}
}
