package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/provider"
)

// TestManagedHermesRestoresBaselineAfterLaterEdit exercises the actual native
// connector; the managed session must replace its usual preserve-later-edit policy.
func TestManagedHermesRestoresBaselineAfterLaterEdit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HERMES_HOME", filepath.Join(home, "hermes"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	dir := filepath.Join(home, "hermes")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("model:\n  default: original\n  provider: own\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := provider.Save(provider.Provider{ID: "deepseek", Name: "DeepSeek", Chat: "https://example.invalid/v1", Key: "fixture", Models: []string{"pro"}}); err != nil {
		t.Fatal(err)
	}
	adapter := HermesManagedAdapter(dir)
	store, err := NewManagedStore(filepath.Join(home, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.BeginManaged("hermes", adapter)
	if err != nil {
		t.Fatal(err)
	}
	err = session.BeforeManagedWrite(func(w *ManagedWrite) error {
		return WithManagedHermes(w, home, func(a *Agent) error { return a.Apply("model", "magpie/deepseek/pro") })
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := edit.SetYAML(path, edit.KV{Path: "model.default", Value: "later"}, edit.KV{Path: "model.provider", Value: "own"}); err != nil {
		t.Fatal(err)
	}
	if err := store.RestoreManaged("hermes", adapter); err != nil {
		t.Fatal(err)
	}
	if got, _ := edit.GetYAML(path, "model.default"); got != "original" {
		t.Errorf("baseline model was not restored: got %q", got)
	}
}

func TestManagedHermesAdapterRestoresRegions(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte("model: null\nproviders:\n  magpie:\n    name: original\n  mine: keep\nagent:\n  reasoning_effort: low\n  max_turns: 7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a := HermesManagedAdapter(root)
	targets := []ManagedTarget{hermesManagedTarget(path, "model"), hermesManagedTarget(path, "providers.magpie"), hermesManagedTarget(path, "agent.reasoning_effort")}
	store, err := NewManagedStore(filepath.Join(t.TempDir(), "journal"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.BeginManaged("hermes", a, targets...)
	if err != nil {
		t.Fatal(err)
	}
	err = session.BeforeManagedWrite(func(w *ManagedWrite) error {
		for _, target := range targets {
			if err := w.BeforeManagedWrite(target); err != nil {
				return err
			}
		}
		return edit.SetYAML(path, edit.KV{Path: "model.default", Value: "changed"}, edit.KV{Path: "providers.magpie.name", Value: "changed"}, edit.KV{Path: "agent.reasoning_effort", Value: "high"}, edit.KV{Path: "agent.max_turns", Value: 9})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.RestoreManaged("hermes", a); err != nil {
		t.Fatal(err)
	}
	if got, _ := edit.GetYAML(path, "model"); got != "null" {
		t.Errorf("model null: %q", got)
	}
	if got, _ := edit.GetYAML(path, "providers.magpie.name"); got != "original" {
		t.Errorf("provider: %q", got)
	}
	if got, _ := edit.GetYAML(path, "agent.reasoning_effort"); got != "low" {
		t.Errorf("effort: %q", got)
	}
	if got, _ := edit.GetYAML(path, "agent.max_turns"); got != "9" {
		t.Errorf("unrelated: %q", got)
	}
}

func TestManagedHermesCaptureFailureWritesNothing(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "hermes")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HERMES_HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	main := filepath.Join(root, "config.yaml")
	bad := filepath.Join(root, "profiles", "bad", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(bad), 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte("model:\n  default: own\n  provider: own\n")
	if err := os.WriteFile(main, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("model: [broken\n"), 0600); err != nil {
		t.Fatal(err)
	}
	adapter := HermesManagedAdapter(root)
	store, err := NewManagedStore(filepath.Join(home, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.BeginManaged("hermes", adapter, hermesManagedTarget(main, "model"))
	if err != nil {
		t.Fatal(err)
	}
	err = session.BeforeManagedWrite(func(w *ManagedWrite) error {
		return WithManagedHermes(w, home, func(a *Agent) error { return a.Apply("model", "magpie/deepseek/pro") })
	})
	if err == nil {
		t.Fatal("malformed profile capture succeeded")
	}
	actual, err := os.ReadFile(main)
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != string(original) {
		t.Fatal("active config changed before capture completed")
	}
}
func TestManagedHermesMissingConfigKeepsLaterComment(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	adapter := HermesManagedAdapter(root)
	store, err := NewManagedStore(filepath.Join(t.TempDir(), "journal"))
	if err != nil {
		t.Fatal(err)
	}
	targets := []ManagedTarget{hermesManagedTarget(path, "model"), hermesManagedTarget(path, "providers.magpie"), hermesManagedTarget(path, "agent.reasoning_effort")}
	session, err := store.BeginManaged("hermes", adapter, targets...)
	if err != nil {
		t.Fatal(err)
	}
	if err = session.BeforeManagedWrite(func(w *ManagedWrite) error {
		return edit.SetYAML(path, edit.KV{Path: "model.default", Value: "temp"}, edit.KV{Path: "providers.magpie.name", Value: "temp"})
	}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("# user comment\n"); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = store.RestoreManaged("hermes", adapter); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("comment file removed")
	}
	if !strings.Contains(string(b), "user comment") {
		t.Fatal("comment lost")
	}
}
func TestManagedHermesCleanupPreservesOtherAgents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	root := filepath.Join(home, "hermes")
	path := filepath.Join(root, "config.yaml")
	stash(map[string]string{"hermes:" + path + ":model.default": "old", "workbuddy:keep": "fixture"})
	record("workbuddy", "model", "keep")
	record("hermes", "model", "magpie/fixture")
	if err := HermesManagedAdapter(root).Cleanup(); err != nil {
		t.Fatal(err)
	}
	if got := stashLoad()["workbuddy:keep"]; got != "fixture" {
		t.Fatal("other stash removed")
	}
	if got := stashLoad()["hermes:"+path+":model.default"]; got != "" {
		t.Fatal("hermes stash remains")
	}
	if got := appliedOf("workbuddy").Fields["model"]; got != "keep" {
		t.Fatal("other applied removed")
	}
	if got := appliedOf("hermes").Fields["model"]; got != "" {
		t.Fatal("hermes applied remains")
	}
}
