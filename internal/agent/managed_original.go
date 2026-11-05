package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"

	"github.com/yetone/magpie/internal/edit"
	"gopkg.in/yaml.v3"
)

// Agent configuration files are small; anything larger is left to the region
// restore rather than copied into the journal.
const managedOriginalLimit = 4 << 20

// originalBytes reads the file as it stood before Magpie's first edit to it.
func originalBytes(path string, exists bool) []byte {
	if !exists {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > managedOriginalLimit {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > managedOriginalLimit {
		return nil
	}
	return raw
}

// restoreOriginalBytes puts the file back exactly as it was, but only when its
// content already matches: restoring a region brings the values back while a
// YAML or JSON re-encode can still change the formatting, and "restored" has to
// mean the file the user had. Content the user changed meanwhile differs, and
// is left alone.
func restoreOriginalBytes(b ManagedFileBaseline, same func(current, original []byte) bool) error {
	if len(b.Original) == 0 || !b.Exists {
		return nil
	}
	current, err := os.ReadFile(b.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if bytes.Equal(current, b.Original) || !same(current, b.Original) {
		return nil
	}
	return edit.WriteAtomic(b.Path, b.Original)
}

// hermesSameDocument compares two YAML documents by value, not by spelling.
func hermesSameDocument(current, original []byte) bool {
	var have, want any
	if yaml.Unmarshal(current, &have) != nil || yaml.Unmarshal(original, &want) != nil {
		return false
	}
	return reflect.DeepEqual(have, want)
}

// workbuddySameModels compares two model lists by value, keeping numbers exact.
func workbuddySameModels(current, original []byte) bool {
	decode := func(raw []byte) (any, bool) {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		var v any
		if d.Decode(&v) != nil {
			return nil, false
		}
		return v, true
	}
	have, ok := decode(current)
	if !ok {
		return false
	}
	want, ok := decode(original)
	if !ok {
		return false
	}
	return reflect.DeepEqual(have, want)
}
