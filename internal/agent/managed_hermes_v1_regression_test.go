package agent

import (
	"encoding/json"
	"github.com/yetone/magpie/internal/edit"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two helpers below are copied from commit 812a227d, so the fixture
// creates its journal and writes the env through the historical code.
func hermes812Capture(t ManagedTarget) (ManagedSnapshot, error) {
	raw, exists, mode, err := hermesManagedRead(t.Path)
	s := ManagedSnapshot{FileExists: exists, Mode: mode}
	if err != nil || !exists {
		return s, err
	}
	if t.Kind == managedHermesEnv {
		records, err := hermesEnvRecords(raw)
		if err != nil {
			return s, err
		}
		var lines []string
		for _, record := range records {
			if record.key == t.Scope {
				lines = append(lines, record.text)
			}
		}
		s.Exists = len(lines) > 0
		if s.Exists {
			encoded, e := json.Marshal(lines)
			if e != nil {
				return s, e
			}
			s.Value = append([]byte("hermes-env-lines-v1:"), encoded...)
		}
		return s, nil
	}
	root, err := hermesYAMLRoot(raw)
	if err != nil {
		return s, err
	}
	parts := strings.Split(t.Scope, ".")
	var r hermesRegion
	parent := root
	if len(parts) == 2 {
		parent = hermesNode(root, parts[0])
		r.ParentExists = parent != nil
		r.ParentMap = parent != nil && parent.Kind == yaml.MappingNode
		if parent != nil && !r.ParentMap {
			r.ParentValue, err = hermesNodeBytes(parent)
			if err != nil {
				return s, err
			}
		}
	}
	n := hermesNode(parent, parts[len(parts)-1])
	s.Exists = n != nil
	r.Value, err = hermesNodeBytes(n)
	if err != nil {
		return s, err
	}
	s.Value, err = json.Marshal(r)
	return s, err
}

func hermes812SetEnv(path, name, value string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	records, err := hermesEnvRecords(raw)
	if err != nil {
		return err
	}
	var out strings.Builder
	for _, r := range records {
		if r.key != name {
			out.WriteString(r.text)
		}
	}
	if err := os.WriteFile(path, []byte(out.String()), 0600); err != nil {
		return err
	}
	return edit.SetEnvFile(path, edit.KV{Path: name, Value: value})
}

func TestManagedHermesV1SingleAssignmentStaysPending(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".env")
	initial := "# key note\nMY_KEY=original # tail\nOTHER=before\n"
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	current := HermesManagedAdapter(root)
	legacy := current
	legacy.Capture = hermes812Capture
	store, err := NewManagedStore(filepath.Join(t.TempDir(), "journal"))
	if err != nil {
		t.Fatal(err)
	}
	target := hermesManagedTarget(path, "MY_KEY")
	session, err := store.BeginManaged("hermes", legacy, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.BeforeManagedWrite(func(w *ManagedWrite) error {
		if err := w.BeforeManagedWrite(target); err != nil {
			return err
		}
		return hermesManagedEdit(path, func(p string) error { return hermes812SetEnv(p, "MY_KEY", "fixture") })
	}); err != nil {
		t.Fatal(err)
	}
	if err := edit.SetEnvFile(path, edit.KV{Path: "OTHER", Value: "later"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RestoreManaged("hermes", current); err == nil {
		t.Fatal("unanchored v1 assignment restored")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("failed restore changed env")
	}
	state, err := store.ManagedState("hermes")
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != ManagedRestoring || state.Pending == 0 {
		t.Fatal("journal did not retain pending restore")
	}
}
