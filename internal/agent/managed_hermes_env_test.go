package agent

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// The target assignment's comment belongs to the user, even when its value is managed.
func TestManagedHermesEnvRestoresAssignmentComment(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".env")
	original := "MY_KEY=original # keep-this-note\nOTHER=before\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	adapter := HermesManagedAdapter(root)
	target := hermesManagedTarget(path, "MY_KEY")
	snap, err := adapter.Capture(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := edit.SetEnvFile(path, edit.KV{Path: "MY_KEY", Value: "fixture-gateway"}); err != nil {
		t.Fatal(err)
	}
	if err := edit.SetEnvFile(path, edit.KV{Path: "OTHER", Value: "later"}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Restore(target, snap); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "MY_KEY=original # keep-this-note") {
		t.Error("original assignment comment lost")
	}
	if got, _ := edit.GetEnvFile(path, "OTHER"); got != "later" {
		t.Error("unrelated value changed")
	}
}

func TestManagedHermesRestoreHardlinkLeavesOtherPath(t *testing.T) {
	for _, tc := range []struct{ name, scope, initial string }{{".env", "MY_KEY", "MY_KEY=original\n"}, {"config.yaml", "model", "model: original\n"}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, tc.name)
			other := filepath.Join(root, "other")
			if err := os.WriteFile(path, []byte(tc.initial), 0600); err != nil {
				t.Fatal(err)
			}
			adapter := HermesManagedAdapter(root)
			target := hermesManagedTarget(path, tc.scope)
			snap, err := adapter.Capture(target)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			later := "MY_KEY=later\n"
			if tc.name == "config.yaml" {
				later = "model: later\n"
			}
			if err := os.WriteFile(other, []byte(later), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(other, path); err != nil {
				t.Fatal(err)
			}
			if err := adapter.Restore(target, snap); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(other)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != later {
				t.Error("restore changed another hardlink")
			}
		})
	}
}

func TestManagedHermesApplyHardlinksLeaveOtherPaths(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "hermes")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HERMES_HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "config.yaml")
	env := filepath.Join(root, ".env")
	cfg := "model:\n  default: own\n  provider: own\nproviders:\n  magpie:\n    api_key: ${MY_KEY}\n"
	if err := os.WriteFile(config, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env, []byte("MY_KEY=fixture-original # keep\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{config, env} {
		if err := os.Link(p, p+".other"); err != nil {
			t.Fatal(err)
		}
	}
	if err := provider.Save(provider.Provider{ID: "deepseek", Name: "DeepSeek", Chat: "https://example.invalid/v1", Key: "fixture", Models: []string{"pro"}}); err != nil {
		t.Fatal(err)
	}
	a := HermesManagedAdapter(root)
	store, err := NewManagedStore(filepath.Join(home, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.BeginManaged("hermes", a, hermesManagedTarget(config, "model"))
	if err != nil {
		t.Fatal(err)
	}
	err = session.BeforeManagedWrite(func(w *ManagedWrite) error {
		return WithManagedHermes(w, home, func(a *Agent) error { return a.Apply("model", "magpie/deepseek/pro") })
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{config, env} {
		b, err := os.ReadFile(p + ".other")
		if err != nil {
			t.Fatal(err)
		}
		expected := cfg
		if p == env {
			expected = "MY_KEY=fixture-original # keep\n"
		}
		if string(b) != expected {
			t.Error("managed apply changed another hardlink")
		}
	}
	// The user replaces each target with another hardlink after joining.
	for _, p := range []string{config, env} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		text := cfg
		if p == env {
			text = "MY_KEY=fixture-later # note\n"
		}
		other := p + ".replacement"
		if err := os.WriteFile(other, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(other, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.BeforeManagedWrite(func(w *ManagedWrite) error {
		return WithManagedHermes(w, home, func(a *Agent) error { return a.Sync() })
	}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{config, env} {
		b, err := os.ReadFile(p + ".replacement")
		if err != nil {
			t.Fatal(err)
		}
		expected := cfg
		if p == env {
			expected = "MY_KEY=fixture-later # note\n"
		}
		if string(b) != expected {
			t.Error("sync changed another hardlink")
		}
	}
	if got, _ := edit.GetYAML(config, "providers.magpie.api_key"); got != "${MY_KEY}" {
		t.Error("sync did not preserve reference")
	}
	if got, _ := edit.GetEnvFile(env, "MY_KEY"); got == "fixture-later" {
		t.Error("sync did not update referenced variable")
	}
	if err := store.RestoreManaged("hermes", a); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{config, env} {
		b, err := os.ReadFile(p + ".replacement")
		if err != nil {
			t.Fatal(err)
		}
		expected := cfg
		if p == env {
			expected = "MY_KEY=fixture-later # note\n"
		}
		if string(b) != expected {
			t.Error("restore changed another hardlink")
		}
	}

	if got, _ := edit.GetEnvFile(env, "MY_KEY"); got != "fixture-original" {
		t.Error("baseline env value not restored")
	}
	if got, _ := edit.GetYAML(config, "model.default"); got != "own" {
		t.Error("baseline model not restored")
	}
}

func TestManagedHermesEnvAssignmentVariants(t *testing.T) {
	for _, original := range []string{"MY_KEY=original # note\n", "MY_KEY=\"a#b\" # note\n", "MY_KEY= # note\n", "MY_KEY=one # first\nMY_KEY=two # last\n", "MY_KEY=\"first\nsecond # inside\" # note\n"} {
		root := t.TempDir()
		path := filepath.Join(root, ".env")
		if err := os.WriteFile(path, []byte(original+"OTHER=initial\n"), 0600); err != nil {
			t.Fatal(err)
		}
		a := HermesManagedAdapter(root)
		target := hermesManagedTarget(path, "MY_KEY")
		snap, err := a.Capture(target)
		if err != nil {
			t.Fatal(err)
		}
		if err := hermesManagedSetEnv(path, "MY_KEY", "fixture"); err != nil {
			t.Fatal(err)
		}
		if err := edit.SetEnvFile(path, edit.KV{Path: "OTHER", Value: "later"}); err != nil {
			t.Fatal(err)
		}
		if err := a.Restore(target, snap); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), original) {
			t.Error("original assignment lost")
		}
		if v, _ := edit.GetEnvFile(path, "OTHER"); v != "later" {
			t.Error("other variable changed")
		}
	}
}

func TestManagedHermesEnvSeparatedAssignmentsKeepOrder(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".env")
	initial := "# first note\nMY_KEY=one # first\n# middle note\nOTHER=${MY_KEY}\nMY_KEY=two # last"
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	a := HermesManagedAdapter(root)
	target := hermesManagedTarget(path, "MY_KEY")
	snap, err := a.Capture(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := hermesManagedSetEnv(path, "MY_KEY", "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := edit.SetEnvFile(path, edit.KV{Path: "OTHER", Value: "later"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Restore(target, snap); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "# first note\nMY_KEY=one # first\n# middle note\nOTHER=later\nMY_KEY=two # last"
	if string(got) != want {
		t.Error("separated assignments or unrelated edit lost their order")
	}
}

func TestManagedHermesEnvPositionConflictRetries(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".env")
	initial := "MY_KEY=one\nOTHER=keep\nMY_KEY=two\n"
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	a := HermesManagedAdapter(root)
	store, err := NewManagedStore(filepath.Join(t.TempDir(), "journal"))
	if err != nil {
		t.Fatal(err)
	}
	target := hermesManagedTarget(path, "MY_KEY")
	session, err := store.BeginManaged("hermes", a, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.BeforeManagedWrite(func(w *ManagedWrite) error { return hermesManagedSetEnv(path, "MY_KEY", "fixture") }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("MY_KEY=fixture\nOTHER=keep\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.RestoreManaged("hermes", a); err == nil {
		t.Fatal("expected retryable position conflict")
	}
	if err := os.WriteFile(path, []byte("MY_KEY=fixture\nOTHER=later\nMY_KEY=fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.RestoreManaged("hermes", a); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "MY_KEY=one\nOTHER=later\nMY_KEY=two\n" {
		t.Error("retry did not restore assignments in order")
	}
}
