package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/steady"
)

var errManagedWorkBuddySchema = errors.New("workbuddy: unknown models.json schema")

// validateManagedWorkBuddy follows the native models file's two supported forms.
// In particular, an unreadable file, null, and empty bytes are never absence.
func validateManagedWorkBuddy(path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("workbuddy: read models.json: %w", err)
	}
	return managedWorkBuddySchema(b)
}

func managedWorkBuddySchema(b []byte) error {
	b = bytes.TrimSpace(bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf}))
	if len(b) == 0 || !utf8.Valid(b) {
		return errManagedWorkBuddySchema
	}
	models := b
	switch b[0] {
	case '[':
	case '{':
		var obj map[string]json.RawMessage
		if json.Unmarshal(b, &obj) != nil {
			return errManagedWorkBuddySchema
		}
		models = obj["models"]
		if raw, ok := obj["availableModels"]; ok {
			var ids []string
			if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '[' || json.Unmarshal(raw, &ids) != nil {
				return errManagedWorkBuddySchema
			}
		}
		if models == nil {
			return nil
		} // {} is a native object with no models yet.
	default:
		return errManagedWorkBuddySchema
	}
	var entries []json.RawMessage
	if len(bytes.TrimSpace(models)) == 0 || bytes.TrimSpace(models)[0] != '[' || json.Unmarshal(models, &entries) != nil {
		return errManagedWorkBuddySchema
	}
	for _, raw := range entries {
		var entry map[string]json.RawMessage
		if json.Unmarshal(raw, &entry) != nil || entry == nil {
			return errManagedWorkBuddySchema
		}
	}
	return nil
}

func managedWorkBuddyTarget(a *Agent) (ManagedTarget, error) {
	if a == nil || a.ID != "workbuddy" || !filepath.IsAbs(a.Dir) || a.Path != filepath.Join(a.Dir, "models.json") {
		return ManagedTarget{}, ErrManagedInvalid
	}
	p, err := managedPath(a.Path)
	if err != nil {
		return ManagedTarget{}, err
	}
	for part := p; ; part = filepath.Dir(part) {
		fi, err := os.Lstat(part)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return ManagedTarget{}, ErrManagedInvalid
		}
		if err == nil && (fi.Mode()&os.ModeIrregular != 0 || part == p && !fi.Mode().IsRegular() || part != p && !fi.IsDir()) {
			return ManagedTarget{}, ErrManagedInvalid
		}
		if filepath.Dir(part) == part {
			break
		}
	}
	return ManagedTarget{Path: p, Scope: "models-file", Kind: "workbuddy-models-bytes-v1"}, nil
}

// ManagedWorkBuddy binds recovery to this native connector's exact models file.
// S8 must drain native writes before restoring and supply its internal marker
// cleanup in adapter.Cleanup; neither hook may call public Disconnect again.
func ManagedWorkBuddy(a *Agent) (ManagedAdapter, []ManagedTarget, error) {
	target, err := managedWorkBuddyTarget(a)
	if err != nil {
		return ManagedAdapter{}, nil, err
	}
	validate := func(t ManagedTarget) error {
		now, err := managedWorkBuddyTarget(a)
		if err != nil || t != target || now != target {
			return ErrManagedInvalid
		}
		return nil
	}
	adapter := ManagedAdapter{Validate: validate}
	adapter.Capture = func(t ManagedTarget) (ManagedSnapshot, error) {
		if err := validate(t); err != nil {
			return ManagedSnapshot{}, err
		}
		if err := managedWorkBuddySingleLink(t.Path); err != nil {
			return ManagedSnapshot{}, err
		}
		fi, err := os.Stat(t.Path)
		if errors.Is(err, os.ErrNotExist) {
			return ManagedSnapshot{}, nil
		}
		if err != nil {
			return ManagedSnapshot{}, err
		}
		b, err := os.ReadFile(t.Path)
		if err != nil {
			return ManagedSnapshot{}, err
		}
		if err := managedWorkBuddySchema(b); err != nil {
			return ManagedSnapshot{}, err
		}
		return ManagedSnapshot{Exists: true, FileExists: true, Mode: uint32(fi.Mode().Perm()), Value: b}, nil
	}
	adapter.Restore = func(t ManagedTarget, snapshot ManagedSnapshot) error {
		if err := validate(t); err != nil {
			return err
		}
		if err := managedWorkBuddySingleLink(t.Path); err != nil {
			return err
		}
		if snapshot.Exists != snapshot.FileExists {
			return ErrManagedInvalid
		}
		if !snapshot.FileExists {
			return removeManagedWorkBuddy(t.Path)
		}
		if err := managedWorkBuddySchema(snapshot.Value); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(t.Path), 0o755); err != nil {
			return err
		}
		defer filememo.Forget()
		return steady.WriteFile(t.Path, snapshot.Value, os.FileMode(snapshot.Mode))
	}
	adapter.Finalize = func(baseline ManagedFileBaseline) error {
		if baseline.Path != target.Path || validate(target) != nil {
			return ErrManagedInvalid
		}
		if err := managedWorkBuddySingleLink(baseline.Path); err != nil {
			return err
		}
		if !baseline.Exists {
			return removeManagedWorkBuddy(baseline.Path)
		}
		return os.Chmod(baseline.Path, os.FileMode(baseline.Mode))
	}
	return adapter, []ManagedTarget{target}, nil
}

// BeforeManagedWorkBuddyWrite runs inside Session.BeforeManagedWrite, immediately
// before the original connector's Set/Sync. Failure must stop that native call.
func BeforeManagedWorkBuddyWrite(w *ManagedWrite, a *Agent) error {
	if w == nil {
		return ErrManagedInvalid
	}
	target, err := managedWorkBuddyTarget(a)
	if err != nil {
		return err
	}
	if err := managedWorkBuddySingleLink(target.Path); err != nil {
		return err
	}
	if err := w.BeforeManagedWrite(target); err != nil {
		return err
	}
	return validateManagedWorkBuddy(target.Path)
}

func removeManagedWorkBuddy(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
