package gateway

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/settings"
)

// Front is the production stack; Handler separately exercises the embedded
// callerGuard without lanGuard identifying the request first. Only the vendor
// is a localhost stand-in. No request can use a real provider or credential.
func TestGatewayKeyStoreFailure(t *testing.T) {
	for _, layer := range []string{"front", "embedded"} {
		for _, kind := range []string{"custom-miss", "custom-hit", "custom-stale", "formatted", "issued"} {
			t.Run(layer+"/"+kind, func(t *testing.T) {
				fresh(t)
				t.Setenv("MAGPIE_ADDR", "")
				calls := limitedUpstream(t, 0)
				secret := "canary-custom-store-unavailable"
				var err error
				if kind == "issued" {
					k, e := access.IssueKey(access.Issue{TTL: time.Hour, ReadOnly: true})
					secret, err = k.Key, e
				} else {
					in := access.Change{Name: "fixture", ReadOnly: true}
					if kind != "formatted" {
						in.Secret = secret
					}
					secret, err = access.Update("add-key", in)
				}
				if err != nil {
					t.Fatal("create fixture:", err)
				}
				if err := settings.Save(settings.Settings{LAN: true}); err != nil {
					t.Fatal(err)
				}
				s := New()
				h := s.Front()
				if layer == "embedded" {
					h = s.Handler()
				}
				ask := func(key string) int {
					return keysCall(h, "POST", "/v1/chat/completions", fromHere, key, chatReq).Code
				}
				if kind == "custom-hit" || kind == "custom-stale" {
					if c := ask(secret); c != http.StatusForbidden {
						t.Fatalf("warm named identity: status %d", c)
					}
				}
				if kind == "custom-stale" {
					// Change the file's size as well as its timestamp. A cache
					// invalidation is deterministic even on coarse filesystems.
					if _, err := access.Update("add-key", access.Change{Name: "invalidate cache"}); err != nil {
						t.Fatal(err)
					}
				}
				before, err := os.ReadFile(access.Path())
				if err != nil {
					t.Fatal(err)
				}
				lock := access.Path() + ".lock"
				if err := os.Remove(lock); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(lock, 0700); err != nil {
					t.Fatal(err)
				}
				var logs bytes.Buffer
				old := log.Writer()
				log.SetOutput(&logs)
				defer log.SetOutput(old)
				w := keysCall(h, "POST", "/v1/chat/completions", fromHere, secret, chatReq)
				if w.Code != http.StatusServiceUnavailable {
					t.Errorf("unavailable key store: status %d, want 503", w.Code)
				}
				if calls.Load() != 0 {
					t.Error("unavailable key store: named request reached vendor anonymously")
				}
				if bytes.Contains(w.Body.Bytes(), []byte(secret)) || bytes.Contains(logs.Bytes(), []byte(secret)) {
					t.Error("credential leaked in error or log")
				}
				// No credential is unambiguously anonymous, even with an
				// unavailable store. A third-party token is checked again once
				// the store is readable, never guessed to be a managed key.
				if c := ask(""); c != http.StatusOK {
					t.Errorf("keyless loopback: status %d", c)
				}
				if err := os.Remove(lock); err != nil {
					t.Fatal(err)
				}
				if c := ask(secret); c != http.StatusForbidden {
					t.Errorf("retry lost read-only identity: status %d", c)
				}
				if c := ask("ordinary-third-party-token"); c != http.StatusOK {
					t.Errorf("ordinary loopback token: status %d", c)
				}
				for _, key := range []string{"", "wrong-third-party-token"} {
					w := keysCall(s.Front(), "GET", "/v1/models", fromThere, key, "")
					if w.Code != http.StatusUnauthorized {
						t.Errorf("remote without enabled key: status %d", w.Code)
					}
				}
				after, err := os.ReadFile(access.Path())
				if err != nil || !bytes.Equal(before, after) {
					t.Error("authentication changed another key or device record")
				}
				who, err := access.Check(secret)
				if err != nil {
					t.Fatal("retry could not identify key")
				}
				if _, err := access.Update("access-key", access.Change{Key: who.KeyID}); err != nil {
					t.Fatal(err)
				}
				if c := ask(secret); c != http.StatusOK {
					t.Fatalf("enabled model call: status %d", c)
				}
				if r := lastUsage(t); r.CallerKeyID != who.KeyID {
					t.Error("retried named call lost usage identity")
				}
			})
		}
	}
}

func TestGatewayKeyStoreUnreadable(t *testing.T) {
	for _, kind := range []string{"custom", "formatted"} {
		for _, failure := range []string{"corrupt", "directory"} {
			t.Run(kind+"/"+failure, func(t *testing.T) {
				fresh(t)
				in := access.Change{Name: "fixture"}
				if kind == "custom" {
					in.Secret = "canary-unreadable-key-store"
				}
				secret, err := access.Update("add-key", in)
				if err != nil {
					t.Fatal(err)
				}
				if failure == "corrupt" {
					if err := os.WriteFile(access.Path(), []byte(`{"canary-unreadable-key-store":`), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Remove(access.Path()); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(access.Path(), 0700); err != nil {
						t.Fatal(err)
					}
				}
				for _, h := range []http.Handler{New().Front(), New().Handler()} {
					w := keysCall(h, "GET", "/v1/models", fromHere, secret, "")
					if w.Code != http.StatusServiceUnavailable {
						t.Errorf("unreadable key store: status %d", w.Code)
					}
					if bytes.Contains(w.Body.Bytes(), []byte(secret)) {
						t.Error("error leaked credential")
					}
				}
			})
		}
	}
}

// A separate OS handle holds the real lock through the production 10s bound.
// This does not shorten access.storeLockWait or simulate a TryLock error.
func TestGatewayKeyStoreHeldLock(t *testing.T) {
	for _, layer := range []string{"front", "embedded"} {
		t.Run(layer, func(t *testing.T) {
			fresh(t)
			t.Setenv("MAGPIE_ADDR", "")
			calls := limitedUpstream(t, 0)
			secret, err := access.Update("add-key", access.Change{Name: "fixture", Secret: "canary-contended-custom-key", ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			h := New().Front()
			if layer == "embedded" {
				h = New().Handler()
			}
			unlock, held := filelock.Lock(access.Path()+".lock", time.Second)
			if !held {
				t.Fatal("could not hold real lock")
			}
			defer func() { unlock() }()
			w := keysCall(h, "POST", "/v1/chat/completions", fromHere, secret, chatReq)
			if w.Code != http.StatusServiceUnavailable || calls.Load() != 0 {
				t.Errorf("real held lock: status %d, vendor calls %d", w.Code, calls.Load())
			}
			unlock()
			unlock = func() {}
			if w := keysCall(h, "POST", "/v1/chat/completions", fromHere, secret, chatReq); w.Code != http.StatusForbidden {
				t.Errorf("retry after lock release lost identity: status %d", w.Code)
			}
		})
	}
}

func TestGatewayDeviceKeyRemainsUnrestricted(t *testing.T) {
	fresh(t)
	calls := limitedUpstream(t, 0)
	c, err := access.EnsureDevice(context.Background(), access.DeviceIdentity{StableNodeID: "fixture-node", NodeID: "1", DisplayName: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	h := New().Front()
	if w := keysCall(h, "POST", "/v1/chat/completions", fromHere, c.Secret, chatReq); w.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("device key: status %d, vendor calls %d", w.Code, calls.Load())
	}
	if r := lastUsage(t); r.CallerKeyID != c.Device.ID {
		t.Error("device request lost usage identity")
	}
}
