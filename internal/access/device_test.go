package access

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDeviceConcurrentIdentityAndRemoval(t *testing.T) {
	oldWait := storeLockWait
	storeLockWait = 60 * time.Second
	t.Cleanup(func() { storeLockWait = oldWait })
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	id := DeviceIdentity{"stable-a", "123", "same"}
	const n = 100
	results := make([]DeviceCredential, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i], errs[i] = EnsureDevice(ctx, id) }(i)
	}
	wg.Wait()
	first := results[0]
	if first.Secret == "" || first.Device.ID == "" {
		t.Fatal("registration did not create credential")
	}
	for i := range n {
		if errs[i] != nil || results[i].Secret != first.Secret || results[i].Device.ID != first.Device.ID {
			t.Fatal("same identity yielded different credentials", errs[i])
		}
	}
	second, e := EnsureDevice(ctx, DeviceIdentity{"stable-b", "456", "same"})
	if e != nil || second.Device.ID == first.Device.ID {
		t.Fatal("distinct nodes merged", e)
	}
	id.DisplayName = "renamed"
	id.NodeID = "789"
	renamed, e := EnsureDevice(ctx, id)
	if e != nil || renamed.Secret != first.Secret || renamed.Device.DisplayName != "renamed" || renamed.Device.NodeID != "789" || renamed.Device.CreatedAt != first.Device.CreatedAt || renamed.Device.LastSeenAt.Before(first.Device.LastSeenAt) {
		t.Fatal("rename failed", e)
	}
	p, e := ListDevices(ctx, "", 1)
	if e != nil || len(p.Items) != 1 || p.NextCursor == nil {
		t.Fatal("pagination failed", e)
	}
	p2, e := ListDevices(ctx, *p.NextCursor, 1)
	if e != nil || len(p2.Items) != 1 || p2.NextCursor != nil || p.Items[0].ID == p2.Items[0].ID {
		t.Fatal("next page failed", e)
	}
	b, _ := json.Marshal(p)
	if strings.Contains(string(b), first.Secret) || strings.Contains(string(b), second.Secret) {
		t.Fatal("list exposed secret")
	}
	keys, e := List()
	if e != nil {
		t.Fatal(e)
	}
	for _, k := range keys {
		if k.Secret != "" {
			t.Fatal("ordinary list exposed secret")
		}
	}
	if e = RemoveDevice(ctx, first.Device.ID); e != nil {
		t.Fatal(e)
	}
	if e = RemoveDevice(ctx, first.Device.ID); e != nil {
		t.Fatal(e)
	}
	again, e := EnsureDevice(ctx, id)
	if e != nil || again.Secret == first.Secret || again.Device.ID == first.Device.ID {
		t.Fatal("removed device did not receive new credential", e)
	}
}
func TestDeviceRestoresUnrestrictedAndKeepsLegacy(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	_, e := Update("add-key", Change{Name: "legacy"})
	if e != nil {
		t.Fatal(e)
	}
	id := DeviceIdentity{"stable", "1", "device"}
	c, e := EnsureDevice(ctx, id)
	if e != nil || c.Secret == "" {
		t.Fatal("registration failed", e)
	}
	mu.Lock()
	keys, e := load()
	if e != nil {
		mu.Unlock()
		t.Fatal(e)
	}
	for i := range keys {
		if keys[i].ID == c.Device.ID {
			keys[i].Off = true
			keys[i].ReadOnly = true
			keys[i].Models = []string{"x"}
			keys[i].Accounts = []string{"y"}
			keys[i].Limit = &Limit{Tokens: 1}
			keys[i].More = []Limit{{Tokens: 1}}
			keys[i].Expires = time.Now()
			keys[i].Issuer = "issuer"
		}
	}
	e = save(keys)
	mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	_, e = EnsureDevice(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	keys, e = List()
	if e != nil || len(keys) != 2 {
		t.Fatal("legacy key lost", e)
	}
	for _, k := range keys {
		if k.ID == c.Device.ID && (k.Off || k.ReadOnly || len(k.Models) > 0 || len(k.Accounts) > 0 || k.Limit != nil || len(k.More) > 0 || !k.Expires.IsZero() || k.Issuer != "") {
			t.Fatal("device remains restricted")
		}
	}
}
func TestDeviceCorruptOrUnreadableStoreNotOverwritten(t *testing.T) {
	for _, kind := range []string{"corrupt", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			if e := os.MkdirAll(filepath.Dir(Path()), 0700); e != nil {
				t.Fatal(e)
			}
			if kind == "corrupt" {
				if e := os.WriteFile(Path(), []byte("broken"), 0600); e != nil {
					t.Fatal(e)
				}
			} else {
				if e := os.Mkdir(Path(), 0700); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := EnsureDevice(context.Background(), DeviceIdentity{"s", "1", "n"}); e == nil {
				t.Fatal("bad store accepted")
			}
			if e := RemoveDevice(context.Background(), "x"); e == nil {
				t.Fatal("bad store removed")
			}
			if kind == "corrupt" {
				b, e := os.ReadFile(Path())
				if e != nil || string(b) != "broken" {
					t.Fatal("corrupt store overwritten")
				}
			}
		})
	}
}
