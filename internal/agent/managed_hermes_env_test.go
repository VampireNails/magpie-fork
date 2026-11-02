package agent

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/provider"
)

func TestManagedHermesEnvAndNewProfile(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "hermes")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HERMES_HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("S7_FIXTURE_KEY", "process-original")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte("model:\n  default: own\n  provider: own\nproviders:\n  magpie:\n    api_key: ${env:S7_FIXTURE_KEY}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := provider.Save(provider.Provider{ID: "deepseek", Name: "DeepSeek", Chat: "https://example.invalid/v1", Key: "fixture", Models: []string{"pro"}}); err != nil {
		t.Fatal(err)
	}
	adapter := HermesManagedAdapter(root)
	store, err := NewManagedStore(filepath.Join(home, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.BeginManaged("hermes", adapter, hermesManagedTarget(path, "model"))
	if err != nil {
		t.Fatal(err)
	}
	run := func(fn func(*Agent) error) {
		t.Helper()
		if err := session.BeforeManagedWrite(func(w *ManagedWrite) error { return WithManagedHermes(w, home, fn) }); err != nil {
			t.Fatal(err)
		}
	}
	run(func(a *Agent) error { return a.Apply("model", "magpie/deepseek/pro") })
	env := filepath.Join(root, ".env")
	if _, err := os.Stat(env); err != nil {
		t.Fatal("expected env write")
	}
	bot := filepath.Join(root, "profiles", "new", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(bot), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bot, []byte("model:\n  model: earlier\n  provider: own\ncustom: keep\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run(func(a *Agent) error { return a.Sync() })
	if got := edit.GetYAMLMap(bot, "providers.magpie"); got == nil {
		t.Fatal("sync did not spread")
	}
	if err := edit.SetYAML(bot, edit.KV{Path: "model.model", Value: "later"}, edit.KV{Path: "custom", Value: "later-unrelated"}); err != nil {
		t.Fatal(err)
	}
	if err := store.RestoreManaged("hermes", adapter); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(env); !os.IsNotExist(err) {
		t.Error("originally missing env remains")
	}
	if got, _ := edit.GetYAML(bot, "model.model"); got != "earlier" {
		t.Errorf("new profile model: %q", got)
	}
	if got, _ := edit.GetYAML(bot, "custom"); got != "later-unrelated" {
		t.Errorf("unrelated: %q", got)
	}
	if got := edit.GetYAMLMap(bot, "providers.magpie"); got != nil {
		t.Error("provider remains")
	}
}

func TestManagedHermesMultipleEnvRegionsRestoreAndRetry(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".env")
	if err := os.WriteFile(path, []byte("ONE=first\nKEEP=untouched\n"), 0600); err != nil {
		t.Fatal(err)
	}
	adapter := HermesManagedAdapter(root)
	store, err := NewManagedStore(filepath.Join(t.TempDir(), "journal"))
	if err != nil {
		t.Fatal(err)
	}
	one := hermesManagedTarget(path, "ONE")
	two := hermesManagedTarget(path, "TWO")
	session, err := store.BeginManaged("hermes", adapter, one)
	if err != nil {
		t.Fatal(err)
	}
	err = session.BeforeManagedWrite(func(w *ManagedWrite) error {
		if err := w.BeforeManagedWrite(one); err != nil {
			return err
		}
		if err := edit.SetEnvFile(path, edit.KV{Path: "ONE", Value: "changed"}); err != nil {
			return err
		}
		if err := w.BeforeManagedWrite(two); err != nil {
			return err
		}
		return edit.SetEnvFile(path, edit.KV{Path: "TWO", Value: "new"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := edit.SetEnvFile(path, edit.KV{Path: "KEEP", Value: "later"}); err != nil {
		t.Fatal(err)
	}
	fail := true
	actual := adapter.Restore
	adapter.Restore = func(tgt ManagedTarget, s ManagedSnapshot) error {
		if tgt.Scope == "TWO" && fail {
			return os.ErrPermission
		}
		return actual(tgt, s)
	}
	if err := store.RestoreManaged("hermes", adapter); err == nil {
		t.Fatal("expected incomplete restore")
	}
	fail = false
	if err := store.RestoreManaged("hermes", adapter); err != nil {
		t.Fatal(err)
	}
	if got, _ := edit.GetEnvFile(path, "ONE"); got != "first" {
		t.Error("first variable not restored")
	}
	if _, ok := edit.GetEnvFile(path, "TWO"); ok {
		t.Error("new variable remains")
	}
	if got, _ := edit.GetEnvFile(path, "KEEP"); got != "later" {
		t.Error("unrelated variable changed")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Errorf("mode %o", info.Mode().Perm())
	}
}

func TestManagedHermesEnvCaptureFailureStopsNativeWrite(t *testing.T) {
	for _, ref := range []string{"${S7_BLOCK_KEY}", "${env:S7_BLOCK_KEY}"} {
		t.Run(ref, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "config.yaml")
			env := filepath.Join(root, ".env")
			t.Setenv("S7_BLOCK_KEY", "fallback-fixture")
			if err := os.WriteFile(path, []byte("providers:\n  magpie:\n    api_key: "+ref+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			sentinel := errors.New("capture failed")
			_, err := hermesKeptKeyScoped(here(root), path, "desired-fixture", func(p, name string) error {
				if p != env || name != "S7_BLOCK_KEY" {
					t.Fatal("wrong target")
				}
				return sentinel
			})
			if !errors.Is(err, sentinel) {
				t.Fatal("capture failure did not propagate")
			}
			if _, err := os.Stat(env); !os.IsNotExist(err) {
				t.Fatal("env created after capture failure")
			}
		})
	}
}
