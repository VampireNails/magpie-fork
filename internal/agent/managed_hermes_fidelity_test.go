package agent

import (
	"os"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"gopkg.in/yaml.v3"
)

func TestManagedHermesCommentFidelityLifecycle(t *testing.T) {
	home := managedEntryHome(t)
	managedEntryConfigured(t)
	a := hermes(home)
	if err := os.MkdirAll(a.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	before := "# header\r\nmodel: # model key\r\n  # original model note\r\n  default: own # default note\r\n  provider: own\r\n  custom: keep # custom note\r\nproviders:\r\n  magpie:\r\n    name: old # provider note\r\n  mine: keep\r\nagent:\r\n  reasoning_effort: low # effort note\r\n  max_turns: 7\r\n"
	if err := os.WriteFile(a.Path, []byte(before), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := NewManagedStore("")
	if err != nil {
		t.Fatal(err)
	}
	m := entryLifecycleEnable(t, store)
	t.Cleanup(func() { _ = m.Close() })
	if err := a.Apply("model", "magpie/fixture/one"); err != nil {
		t.Fatal(err)
	}
	if err := edit.SetYAML(a.Path, edit.KV{Path: "agent.max_turns", Value: 9}); err != nil {
		t.Fatal(err)
	}
	if err := a.Disconnect(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# header", "# model key", "# original model note", "# default note", "# custom note", "# provider note", "# effort note"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("lost %q; restored:\n%s", want, raw)
		}
	}
	for key, want := range map[string]string{"model.default": "own", "model.provider": "own", "model.custom": "keep", "providers.magpie.name": "old", "providers.mine": "keep", "agent.reasoning_effort": "low", "agent.max_turns": "9"} {
		if got, _ := edit.GetYAML(a.Path, key); got != want {
			t.Errorf("%s = %q, want %q; restored:\n%s", key, got, want, raw)
		}
	}
	t.Logf("before:\n%s\nafter:\n%s", before, raw)
}

func TestManagedHermesAliasLifecycle(t *testing.T) {
	home := managedEntryHome(t)
	managedEntryConfigured(t)
	a := hermes(home)
	if err := os.MkdirAll(a.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	before := "defaults: &saved\n  default: own\n  provider: own\nmodel: *saved # shared model\nother: keep\n"
	if err := os.WriteFile(a.Path, []byte(before), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := NewManagedStore("")
	if err != nil {
		t.Fatal(err)
	}
	m := entryLifecycleEnable(t, store)
	t.Cleanup(func() { _ = m.Close() })
	if err := a.Apply("model", "magpie/fixture/one"); err != nil {
		t.Fatal(err)
	}
	if err := a.Disconnect(); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	raw, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Model struct {
			Default  string `yaml:"default"`
			Provider string `yaml:"provider"`
		} `yaml:"model"`
		Other string `yaml:"other"`
	}
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model.Default != "own" || got.Model.Provider != "own" || got.Other != "keep" || !strings.Contains(string(raw), "*saved") {
		t.Errorf("alias not restored: %s", raw)
	}
	t.Logf("before:\n%s\nafter:\n%s", before, raw)
}

func TestManagedHermesAliasMissingAnchorRetries(t *testing.T) {
	home := managedEntryHome(t)
	managedEntryConfigured(t)
	a := hermes(home)
	if err := os.MkdirAll(a.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	before := "defaults: &saved {default: own, provider: own}\nmodel: *saved\nother: keep\n"
	if err := os.WriteFile(a.Path, []byte(before), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := NewManagedStore("")
	if err != nil {
		t.Fatal(err)
	}
	m := entryLifecycleEnable(t, store)
	t.Cleanup(func() { _ = m.Close() })
	if err := a.Apply("model", "magpie/fixture/one"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(raw), "&saved", "", 1)
	if changed == string(raw) {
		t.Fatalf("anchor missing after apply: %s", raw)
	}
	if err := os.WriteFile(a.Path, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.Disconnect(); err == nil {
		t.Fatal("restore without anchor succeeded")
	}
	state, err := store.ManagedState("hermes")
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase == ManagedDetached {
		t.Fatal("failed restore discarded journal")
	}
	raw, err = os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	changed = strings.Replace(string(raw), "defaults:", "defaults: &saved", 1)
	if err := os.WriteFile(a.Path, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.Disconnect(); err != nil {
		t.Fatalf("retry: %v", err)
	}
	raw, err = os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "*saved") || !strings.Contains(string(raw), "other: keep") {
		t.Fatalf("retry lost config: %s", raw)
	}
}

func TestManagedHermesNestedExternalAliasCaptureFailsBeforeWrite(t *testing.T) {
	home := managedEntryHome(t)
	managedEntryConfigured(t)
	a := hermes(home)
	if err := os.MkdirAll(a.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	before := "shared: &saved own\nmodel:\n  default: *saved\n  provider: own\nother: keep\n"
	if err := os.WriteFile(a.Path, []byte(before), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := NewManagedStore("")
	if err != nil {
		t.Fatal(err)
	}
	m := entryLifecycleEnable(t, store)
	t.Cleanup(func() { _ = m.Close() })
	if err := a.Apply("model", "magpie/fixture/one"); err == nil {
		t.Fatal("capture accepted unresolvable nested alias")
	}
	raw, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != before {
		t.Fatalf("capture failure modified config: %s", raw)
	}
}

func TestManagedHermesParentAliasLifecycle(t *testing.T) {
	home := managedEntryHome(t)
	managedEntryConfigured(t)
	a := hermes(home)
	if err := os.MkdirAll(a.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	before := "shared: &saved {mine: keep}\nproviders: *saved\nmodel: {default: own, provider: own}\n"
	if err := os.WriteFile(a.Path, []byte(before), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := NewManagedStore("")
	if err != nil {
		t.Fatal(err)
	}
	m := entryLifecycleEnable(t, store)
	t.Cleanup(func() { _ = m.Close() })
	if err := a.Apply("model", "magpie/fixture/one"); err != nil {
		t.Fatal(err)
	}
	if err := a.Disconnect(); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	raw, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Providers map[string]string `yaml:"providers"`
	}
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Providers["mine"] != "keep" || !strings.Contains(string(raw), "*saved") {
		t.Fatalf("parent alias not restored: %s", raw)
	}
}
