package agent

import (
	"bytes"
	"os"
	"testing"
)

const prettyHermesConfig = `{
  "terminal": {
    "backend": "local",
    "cwd": "/data/home/hermes-workspace",
    "timeout": 180
  },
  "agent": {
    "max_turns": 30
  },
  "model": {},
  "_config_version": 52
}
`

// A region restore brings the values back; the re-encode can still change the
// spelling. The file the user had comes back byte for byte.
func TestManagedHermesRestoresOriginalBytes(t *testing.T) {
	home := managedEntryHome(t)
	managedEntryConfigured(t)
	a := hermes(home)
	if err := os.MkdirAll(a.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.Path, []byte(prettyHermesConfig), 0o600); err != nil {
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
	wired, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(wired, []byte(prettyHermesConfig)) {
		t.Fatal("wiring did not change the file; the test would prove nothing")
	}
	if err := a.Disconnect(); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, []byte(prettyHermesConfig)) {
		t.Fatalf("restored file differs from the original bytes:\n--- want ---\n%s\n--- got ---\n%s", prettyHermesConfig, restored)
	}
}

// A value the user changed while wired is not ours to overwrite: the file stays
// as the user left it, even though it no longer matches the original.
func TestManagedHermesKeepsUserEditsWhileWired(t *testing.T) {
	home := managedEntryHome(t)
	managedEntryConfigured(t)
	a := hermes(home)
	if err := os.MkdirAll(a.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.Path, []byte(prettyHermesConfig), 0o600); err != nil {
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
	edited := []byte(`{"agent": {"max_turns": 77}, "model": {}, "_config_version": 52}` + "\n")
	if err := os.WriteFile(a.Path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Disconnect(); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(restored, []byte(prettyHermesConfig)) {
		t.Fatal("the user's own edit was replaced by the original bytes")
	}
	if !bytes.Contains(restored, []byte("77")) {
		t.Fatalf("the user's edit was lost:\n%s", restored)
	}
}

func TestRestoreOriginalBytesSkipsUnreadableAndMissing(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yaml"
	same := func(a, b []byte) bool { return bytes.Equal(a, b) }
	if err := restoreOriginalBytes(ManagedFileBaseline{Path: path, Exists: true, Original: []byte("a")}, same); err != nil {
		if !os.IsNotExist(err) {
			t.Fatalf("missing file: %v", err)
		}
	}
	if err := os.WriteFile(path, []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreOriginalBytes(ManagedFileBaseline{Path: path, Exists: true, Original: []byte("a\n")}, func(a, b []byte) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "b" {
		t.Fatalf("content changed without consent: %s", raw)
	}
	// Same value, different spelling: the original bytes win.
	sameValue := func(a, b []byte) bool { return bytes.TrimSpace(a)[0] == bytes.TrimSpace(b)[0] }
	if err := restoreOriginalBytes(ManagedFileBaseline{Path: path, Exists: true, Original: []byte("b\n")}, sameValue); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "b\n" {
		t.Fatalf("original bytes not written: %q", raw)
	}
}
