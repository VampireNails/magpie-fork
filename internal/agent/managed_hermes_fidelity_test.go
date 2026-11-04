package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	if got.Model.Default != "own" || got.Model.Provider != "own" || got.Other != "keep" || !strings.Contains(string(raw), "model: *saved # shared model") || strings.Count(string(raw), "# shared model") != 1 {
		t.Errorf("alias not restored: %s", raw)
	}
	t.Logf("before:\n%s\nafter:\n%s", before, raw)
}

func TestManagedHermesMissingAnchorMaterializesSnapshot(t *testing.T) {
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
		t.Fatal("anchor missing before edit")
	}
	if err := os.WriteFile(a.Path, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.Disconnect(); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Model map[string]string `yaml:"model"`
		Other string            `yaml:"other"`
	}
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model["default"] != "own" || got.Other != "keep" {
		t.Fatalf("lost snapshot or unrelated field: %s", raw)
	}
}

func TestManagedHermesNestedExternalAliasSnapshot(t *testing.T) {
	home := managedEntryHome(t)
	managedEntryConfigured(t)
	a := hermes(home)
	if err := os.MkdirAll(a.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	before := "shared: &saved own\nmodel:\n  default: *saved # default note\n  provider: own\nother: keep # other note\n"
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
	changed := strings.Replace(string(raw), "&saved own", "&saved changed", 1)
	if changed == string(raw) {
		t.Fatalf("anchor missing: %s", raw)
	}
	if err := os.WriteFile(a.Path, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.Disconnect(); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Shared string            `yaml:"shared"`
		Model  map[string]string `yaml:"model"`
	}
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Shared != "changed" || got.Model["default"] != "own" {
		t.Errorf("lost values: %s", raw)
	}
	if !strings.Contains(string(raw), "default: own # default note") || !strings.Contains(string(raw), "other: keep # other note") || strings.Count(string(raw), "# default note") != 1 || strings.Count(string(raw), "# other note") != 1 {
		t.Errorf("comments moved or duplicated: %s", raw)
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

func TestManagedHermesAliasSnapshotChangedAnchor(t *testing.T) {
	home := managedEntryHome(t)
	managedEntryConfigured(t)
	a := hermes(home)
	if err := os.MkdirAll(a.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	before := "defaults: &saved\n  default: old\n  provider: own\nmodel: *saved # model note\nother: keep # other note\n"
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
	changed := strings.Replace(string(raw), "default: old", "default: changed", 1)
	if changed == string(raw) {
		t.Fatalf("missing anchor: %s", raw)
	}
	if err := os.WriteFile(a.Path, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("before disconnect:\n%s", changed)
	if err := a.Disconnect(); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Defaults map[string]string `yaml:"defaults"`
		Model    map[string]string `yaml:"model"`
	}
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model["default"] != "old" || got.Defaults["default"] != "changed" {
		t.Errorf("values: %s", raw)
	}
	if strings.Count(string(raw), "# model note") != 1 || !strings.Contains(string(raw), "model: # model note") || strings.Count(string(raw), "# other note") != 1 || !strings.Contains(string(raw), "other: keep # other note") {
		t.Errorf("comments duplicated/lost: %s", raw)
	}
}

func TestManagedHermesDuplicateAnchorMaterializesSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	target := hermesManagedTarget(path, "model")
	original := "first: &saved {default: old}\nmodel: *saved\nother: keep\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	snap, err := hermesManagedCapture(target)
	if err != nil {
		t.Fatal(err)
	}
	changed := "first: &saved {default: old}\nsecond: &saved {default: changed}\nmodel: {default: managed}\nother: keep\n"
	if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	if err := hermesManagedRestore(target, snap); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var got struct {
		Second map[string]string `yaml:"second"`
		Model  map[string]string `yaml:"model"`
	}
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Second["default"] != "changed" || got.Model["default"] != "old" {
		t.Fatalf("wrong anchor binding: %s", raw)
	}
}

func TestManagedHermesLegacyAliasWithoutSnapshotFailsSafely(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	target := hermesManagedTarget(path, "model")
	current := "saved: &old {default: changed}\nmodel: {default: managed}\n"
	if err := os.WriteFile(path, []byte(current), 0600); err != nil {
		t.Fatal(err)
	}
	value, err := json.Marshal(hermesRegion{Alias: "old", Value: []byte("*old\n")})
	if err != nil {
		t.Fatal(err)
	}
	if err := hermesManagedRestore(target, ManagedSnapshot{FileExists: true, Exists: true, Value: value}); err == nil {
		t.Fatal("legacy snapshot without a value restored silently")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != current {
		t.Fatalf("failure changed file: %s", raw)
	}
}

func TestManagedHermesRestoreDoesNotBreakUnrelatedAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	target := hermesManagedTarget(path, "model")
	if err := os.WriteFile(path, []byte("model: {default: old}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	snap, err := hermesManagedCapture(target)
	if err != nil {
		t.Fatal(err)
	}
	current := "model: &live {default: managed}\nother: *live\n"
	if err := os.WriteFile(path, []byte(current), 0600); err != nil {
		t.Fatal(err)
	}
	if err := hermesManagedRestore(target, snap); err == nil {
		t.Fatal("restore broke an unrelated alias")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != current {
		t.Fatalf("failure changed file: %s", raw)
	}
}

func TestManagedHermesNestedAliasTargetCommentsNotCopied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	target := hermesManagedTarget(path, "model")
	original := "shared: &base old\ndefaults: &saved\n  default: *base # target note\nmodel: *saved # model note\nother: keep # other note\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	snap, err := hermesManagedCapture(target)
	if err != nil {
		t.Fatal(err)
	}
	current := "shared: &base changed\ndefaults: &saved\n  default: *base # target note\nmodel: # model note\n  default: managed\nother: keep # other note\n"
	if err := os.WriteFile(path, []byte(current), 0600); err != nil {
		t.Fatal(err)
	}
	if err := hermesManagedRestore(target, snap); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Count(string(raw), "# target note") != 1 || !strings.Contains(string(raw), "default: *base # target note") || strings.Count(string(raw), "# model note") != 1 || strings.Count(string(raw), "# other note") != 1 {
		t.Fatalf("comments copied or moved: %s", raw)
	}
	var got struct {
		Model  map[string]string `yaml:"model"`
		Shared string            `yaml:"shared"`
	}
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model["default"] != "old" || got.Shared != "changed" {
		t.Fatalf("values: %s", raw)
	}
}

func TestManagedHermesParentAliasMergeLifecycle(t *testing.T) {
	home := managedEntryHome(t)
	managedEntryConfigured(t)
	a := hermes(home)
	if err := os.MkdirAll(a.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	before := "shared: &saved {magpie: {name: old}, mine: old}\nproviders: *saved # parent note\nmodel: {default: own, provider: own}\n"
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
	current := "shared: &saved {magpie: {name: changed}, mine: changed}\nproviders: {magpie: {name: managed}, added: keep}\nmodel: {default: managed, provider: magpie}\n"
	if err := os.WriteFile(a.Path, []byte(current), 0600); err != nil {
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
		Shared    map[string]any `yaml:"shared"`
		Providers map[string]any `yaml:"providers"`
	}
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Shared["mine"] != "changed" || got.Providers["mine"] != "old" || got.Providers["added"] != "keep" || got.Providers["magpie"].(map[string]any)["name"] != "old" {
		t.Fatalf("wrong merged values: %s", raw)
	}
}
func TestManagedHermesParentAliasMergeRetry(t *testing.T) {
	home := managedEntryHome(t)
	managedEntryConfigured(t)
	a := hermes(home)
	if err := os.MkdirAll(a.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	original := "shared: &saved {magpie: {name: old}, mine: {nested: old, remove: old}}\nproviders: *saved # parent note\nmodel: {default: own, provider: own}\n"
	if err := os.WriteFile(a.Path, []byte(original), 0600); err != nil {
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
	malformed := "providers: [broken\n"
	if err := os.WriteFile(a.Path, []byte(malformed), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.Disconnect(); err == nil {
		t.Fatal("invalid document accepted")
	}
	raw, _ := os.ReadFile(a.Path)
	if string(raw) != malformed {
		t.Fatalf("failed restore changed file: %s", raw)
	}
	current := "shared: &saved {magpie: {name: changed}, mine: {nested: changed}} # anchor note\nother: &other value # other note\nproviders: # parent note\n  magpie: {name: managed}\n  mine: {nested: edited} # sibling note\n  added: *other\nmodel: {default: managed, provider: magpie}\n"
	if err := os.WriteFile(a.Path, []byte(current), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.Disconnect(); err != nil {
		t.Fatalf("retry: %v", err)
	}
	raw, err = os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Providers struct {
			Magpie map[string]string `yaml:"magpie"`
			Mine   map[string]string `yaml:"mine"`
			Added  string            `yaml:"added"`
		} `yaml:"providers"`
	}
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Providers.Magpie["name"] != "old" || got.Providers.Mine["nested"] != "edited" || got.Providers.Mine["remove"] != "" || got.Providers.Added != "value" {
		t.Fatalf("wrong merged values: %s", raw)
	}
	for _, comment := range []string{"# parent note", "# anchor note", "# other note", "# sibling note"} {
		if strings.Count(string(raw), comment) != 1 {
			t.Fatalf("lost or duplicated %s: %s", comment, raw)
		}
	}
}

func TestManagedHermesAliasParentCommentOwnership(t *testing.T) {
	oldMark := edit.Mark
	edit.Mark = func(table, key, value string) string {
		if table == "providers.magpie" && key == "base_url" {
			return "managed gateway note"
		}
		return ""
	}
	t.Cleanup(func() { edit.Mark = oldMark })
	home := managedEntryHome(t)
	managedEntryConfigured(t)
	a := hermes(home)
	if err := os.MkdirAll(a.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	before := "shared: &saved\n  mine:\n    name: original\nproviders: *saved # parent baseline\nmodel:\n  default: original-model\n  provider: original\nother: keep # unrelated baseline\n"
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
	s := strings.Replace(string(raw), "name: original", "name: changed", 1)
	s = strings.Replace(s, "providers: # parent baseline\n", "providers: # parent baseline\n  added: keep\n", 1)
	if s == string(raw) || !strings.Contains(s, "added: keep") {
		t.Fatalf("fixture setup: %s", s)
	}
	if err := os.WriteFile(a.Path, []byte(s), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.Disconnect(); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if strings.Count(got, "# parent baseline") != 1 || !strings.Contains(got, "providers: # parent baseline") || strings.Contains(got, "# magpie:") || !strings.Contains(got, "other: keep # unrelated baseline") {
		t.Errorf("comments moved, duplicated, or retained:\n%s", got)
	}
	for key, want := range map[string]string{"shared.mine.name": "changed", "providers.mine.name": "original", "providers.added": "keep", "model.default": "original-model", "model.provider": "original", "other": "keep"} {
		if actual, _ := edit.GetYAML(a.Path, key); actual != want {
			t.Errorf("%s = %q, want %q", key, actual, want)
		}
	}
}
