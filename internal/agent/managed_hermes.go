package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/steady"
	"gopkg.in/yaml.v3"
)

const managedHermesYAML = "hermes-yaml-v1"
const managedHermesEnv = "hermes-env-v1"

var managedHermesEnvLine = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=`)

var managedHermesVar = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type hermesRegion struct {
	ParentExists bool   `json:"parentExists"`
	ParentValue  []byte `json:"parentValue"`
	ParentMap    bool   `json:"parentMap"`
	Value        []byte `json:"value"`
}

// HermesManagedAdapter owns only Hermes configuration regions under root.
// It deliberately does not invoke native Disconnect during restoration: that
// would introduce new profile writes and restore its own conditional stash.
func HermesManagedAdapter(root string) ManagedAdapter {
	root = filepath.Clean(root)
	validate := func(t ManagedTarget) error {
		if !filepath.IsAbs(root) || !filepath.IsAbs(t.Path) {
			return ErrManagedInvalid
		}
		dir := filepath.Dir(t.Path)
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			return ErrManagedInvalid
		}
		if rel != "." {
			p := strings.Split(rel, string(filepath.Separator))
			if len(p) != 2 || p[0] != "profiles" || !hermesProfileID.MatchString(p[1]) || p[1] == "default" {
				return ErrManagedInvalid
			}
		}
		switch t.Kind {
		case managedHermesYAML:
			if filepath.Base(t.Path) != "config.yaml" || (t.Scope != "model" && t.Scope != "providers.magpie" && t.Scope != "agent.reasoning_effort") {
				return ErrManagedInvalid
			}
		case managedHermesEnv:
			if filepath.Base(t.Path) != ".env" || !managedHermesVar.MatchString(t.Scope) {
				return ErrManagedInvalid
			}
		default:
			return ErrManagedInvalid
		}
		return nil
	}
	return ManagedAdapter{Validate: validate, Capture: hermesManagedCapture, Restore: hermesManagedRestore, Finalize: hermesManagedFinalize, Cleanup: func() error { return hermesManagedCleanup(root) }}
}

func hermesManagedTarget(path, scope string) ManagedTarget {
	kind := managedHermesYAML
	if filepath.Base(path) == ".env" {
		kind = managedHermesEnv
	}
	return ManagedTarget{Path: filepath.Clean(path), Scope: scope, Kind: kind}
}

func hermesManagedRead(path string) ([]byte, bool, uint32, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, 0, nil
	}
	if err != nil || !fi.Mode().IsRegular() {
		return nil, false, 0, ErrManagedCapture
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false, 0, err
	}
	return b, true, uint32(fi.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)), nil
}

func hermesYAMLRoot(raw []byte) (*yaml.Node, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, nil
	}
	var doc yaml.Node
	d := yaml.NewDecoder(bytes.NewReader(raw))
	if err := d.Decode(&doc); err != nil {
		return nil, err
	}
	var extra yaml.Node
	if err := d.Decode(&extra); err == nil {
		return nil, ErrManagedCapture
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", HeadComment: doc.HeadComment, FootComment: doc.FootComment}, nil
	}
	if len(doc.Content) == 1 && doc.Content[0].Kind == yaml.ScalarNode && doc.Content[0].Tag == "!!null" && doc.Content[0].Value == "" {
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", HeadComment: doc.HeadComment, FootComment: doc.FootComment}, nil
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, ErrManagedCapture
	}
	var check any
	if err := doc.Content[0].Decode(&check); err != nil {
		return nil, err
	}
	if doc.HeadComment != "" && doc.Content[0].HeadComment == "" {
		doc.Content[0].HeadComment = doc.HeadComment
	}
	if doc.FootComment != "" && doc.Content[0].FootComment == "" {
		doc.Content[0].FootComment = doc.FootComment
	}
	return doc.Content[0], nil
}
func hermesNode(n *yaml.Node, k string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == k {
			return n.Content[i+1]
		}
	}
	return nil
}
func hermesNodeBytes(n *yaml.Node) ([]byte, error) {
	if n == nil {
		return nil, nil
	}
	return yaml.Marshal(n)
}

func hermesManagedCapture(t ManagedTarget) (ManagedSnapshot, error) {
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
		var saved hermesEnvSnapshot
		counts := map[string]int{}
		for _, record := range records {
			if record.key == t.Scope {
				saved.Lines = append(saved.Lines, record.text)
				continue
			}
			if anchor := hermesEnvAnchor(record); anchor != "" {
				counts[anchor]++
				saved.Anchors = append(saved.Anchors, hermesEnvAnchorAt{ID: anchor, Before: len(saved.Lines)})
			}
		}
		s.Exists = len(saved.Lines) > 0
		if s.Exists {
			for _, n := range counts {
				if n != 1 {
					return s, ErrManagedCapture
				}
			}
			encoded, e := json.Marshal(saved)
			if e != nil {
				return s, e
			}
			s.Value = append([]byte("hermes-env-anchors-v2:"), encoded...)
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

func hermesManagedRestore(t ManagedTarget, s ManagedSnapshot) error {
	return hermesManagedEdit(t.Path, func(path string) error { t.Path = path; return hermesManagedRestoreStaged(t, s) })
}

func hermesManagedRestoreStaged(t ManagedTarget, s ManagedSnapshot) error {
	if t.Kind != managedHermesYAML {
		return hermesManagedRestoreRegion(t, s)
	}
	raw, exists, _, err := hermesManagedRead(t.Path)
	if err != nil {
		return err
	}
	var comments []string
	if exists {
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				comments = append(comments, strings.TrimRight(line, "\r"))
			}
		}
	}
	if err := hermesManagedRestoreRegion(t, s); err != nil {
		return err
	}
	if len(comments) == 0 {
		return nil
	}
	current, exists, _, err := hermesManagedRead(t.Path)
	if err != nil {
		return err
	}
	present := map[string]int{}
	for _, line := range strings.Split(string(current), "\n") {
		present[strings.TrimRight(line, "\r")]++
	}
	var missing []string
	for _, line := range comments {
		if present[line] > 0 {
			present[line]--
		} else {
			missing = append(missing, line)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if !exists {
		current = nil
	}
	if len(current) > 0 && current[len(current)-1] != '\n' {
		current = append(current, '\n')
	}
	current = append(current, []byte(strings.Join(missing, "\n")+"\n")...)
	return edit.WriteAtomic(t.Path, current)
}

func hermesManagedRestoreRegion(t ManagedTarget, s ManagedSnapshot) error {
	if t.Kind == managedHermesEnv {
		const oldPrefix = "hermes-env-lines-v1:"
		const prefix = "hermes-env-anchors-v2:"
		var original []string
		var saved hermesEnvSnapshot
		anchored := s.Exists && bytes.HasPrefix(s.Value, []byte(prefix))
		if anchored {
			if json.Unmarshal(s.Value[len(prefix):], &saved) != nil {
				return ErrManagedRestore
			}
			original = saved.Lines
		} else if s.Exists && bytes.HasPrefix(s.Value, []byte(oldPrefix)) {
			return ErrManagedRestore
		} else if s.Exists {
			return edit.SetEnvFile(t.Path, edit.KV{Path: t.Scope, Value: string(s.Value)})
		}
		raw, exists, _, err := hermesManagedRead(t.Path)
		if err != nil {
			return err
		}
		if !exists && !s.Exists {
			return nil
		}
		records, err := hermesEnvRecords(raw)
		if err != nil {
			return err
		}
		if anchored && !hermesEnvAnchorsMatch(records, t.Scope, saved.Anchors) {
			return ErrManagedRestore
		}
		var out strings.Builder
		count := 0
		for _, record := range records {
			if record.key == t.Scope {
				count++
			}
		}
		if s.Exists && count != len(original) {
			return ErrManagedRestore
		}
		n := 0
		for i, record := range records {
			if record.key != t.Scope {
				out.WriteString(record.text)
				continue
			}
			if s.Exists {
				saved := original[n]
				n++
				out.WriteString(saved)
				if !strings.HasSuffix(saved, "\n") && i+1 < len(records) && records[i+1].text != "" {
					out.WriteByte('\n')
				}
			}
		}
		return edit.WriteAtomic(t.Path, []byte(out.String()))
	}
	var r hermesRegion
	if s.FileExists && json.Unmarshal(s.Value, &r) != nil {
		return ErrManagedRestore
	}
	parts := strings.Split(t.Scope, ".")
	if len(parts) == 2 && r.ParentExists && !r.ParentMap {
		raw, exists, _, err := hermesManagedRead(t.Path)
		if err != nil {
			return err
		}
		if exists {
			root, e := hermesYAMLRoot(raw)
			if e != nil {
				return e
			}
			p := hermesNode(root, parts[0])
			if p != nil && p.Kind == yaml.MappingNode {
				for i := 0; i+1 < len(p.Content); i += 2 {
					if p.Content[i].Value != parts[1] {
						return ErrManagedRestore
					}
				}
			}
		}
		return edit.SetYAML(t.Path, edit.KV{Path: parts[0], Value: edit.YAMLText(r.ParentValue)})
	}
	if s.Exists {
		return edit.SetYAML(t.Path, edit.KV{Path: t.Scope, Value: edit.YAMLText(r.Value)})
	}
	if err := edit.DelYAML(t.Path, t.Scope); err != nil {
		return err
	}
	if len(parts) == 2 && !r.ParentExists {
		raw, exists, _, err := hermesManagedRead(t.Path)
		if err != nil || !exists {
			return err
		}
		root, err := hermesYAMLRoot(raw)
		if err != nil {
			return err
		}
		parent := hermesNode(root, parts[0])
		if parent != nil && parent.Kind == yaml.MappingNode && len(parent.Content) == 0 {
			return edit.DelYAML(t.Path, parts[0])
		}
	}
	return nil
}

func hermesManagedFinalize(f ManagedFileBaseline) error {
	raw, exists, _, err := hermesManagedRead(f.Path)
	if err != nil {
		return err
	}
	if !exists {
		if !f.Exists {
			return nil
		}
		if err := edit.WriteAtomic(f.Path, []byte{}); err != nil {
			return err
		}
	}
	if !f.Exists && exists {
		empty := len(bytes.TrimSpace(raw)) == 0
		if filepath.Base(f.Path) == "config.yaml" {
			root, e := hermesYAMLRoot(raw)
			if e != nil {
				return e
			}
			empty = len(root.Content) == 0 && root.HeadComment == "" && root.LineComment == "" && root.FootComment == ""
		}
		if empty {
			return os.Remove(f.Path)
		}
	}
	if f.Exists {
		return hermesManagedEdit(f.Path, func(p string) error { return os.Chmod(p, os.FileMode(f.Mode)) })
	}
	return nil
}

// WithManagedHermes constructs a connector bound to a single synchronous write
// callback. The connector and scope must not escape the callback or cross a goroutine.
// The caller holds the S8 operation guard before entering the managed session.
func WithManagedHermes(w *ManagedWrite, home string, fn func(*Agent) error) error {
	if w == nil || fn == nil {
		return ErrManagedInvalid
	}
	at := here(home)
	before := func(path string) error {
		for _, scope := range []string{"model", "providers.magpie", "agent.reasoning_effort"} {
			if err := w.BeforeManagedWrite(hermesManagedTarget(path, scope)); err != nil {
				return err
			}
		}
		return nil
	}
	paths, err := managedHermesProfiles(at)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := before(path); err != nil {
			return err
		}
	}
	beforeEnv := func(path, name string) error {
		if !managedHermesVar.MatchString(name) {
			return ErrManagedInvalid
		}
		return w.BeforeManagedWrite(hermesManagedTarget(path, name))
	}
	return fn(hermesInScoped(at, before, beforeEnv))
}

func managedHermesProfiles(at place) ([]string, error) {
	root := hermesRoot(at)
	paths := []string{filepath.Join(hermesActive(at), "config.yaml")}
	rootPath := filepath.Join(root, "config.yaml")
	if filepath.Clean(rootPath) != filepath.Clean(paths[0]) {
		paths = append(paths, rootPath)
	}
	dir := filepath.Join(root, "profiles")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return paths, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "default" || !hermesProfileID.MatchString(name) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			continue
		}
		_, err = os.Stat(filepath.Join(dir, ".deleted", name))
		if err == nil {
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		path := filepath.Join(dir, name, "config.yaml")
		info, err = os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, ErrManagedInvalid
		}
		if filepath.Clean(path) != filepath.Clean(paths[0]) {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

// HermesManagedRoot is the native profile root for this machine and home.
func HermesManagedRoot(home string) string { return hermesRoot(here(home)) }

// HermesManagedInitialTargets identifies the active config's regions before
// BeginManaged; WithManagedHermes captures the remaining and later profiles.
func HermesManagedInitialTargets(home string) []ManagedTarget {
	path := filepath.Join(hermesActive(here(home)), "config.yaml")
	return []ManagedTarget{hermesManagedTarget(path, "model"), hermesManagedTarget(path, "providers.magpie"), hermesManagedTarget(path, "agent.reasoning_effort")}
}

// hermesManagedCleanup removes this connector's transient bookkeeping without
// invoking Disconnect or replacing entries owned by another agent.
func hermesManagedCleanup(root string) error {
	stashMu.Lock()
	err := func() error {
		path := stashPath()
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		var values map[string]string
		if json.Unmarshal(raw, &values) != nil || values == nil {
			return ErrManagedRestore
		}
		for key := range values {
			if strings.HasPrefix(key, "hermes:") {
				rest := strings.TrimPrefix(key, "hermes:")
				for _, suffix := range []string{":model.provider", ":model.default", ":magpie.default", ":picked", ":off"} {
					if strings.HasSuffix(rest, suffix) {
						p := strings.TrimSuffix(rest, suffix)
						rel, e := filepath.Rel(root, p)
						if e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
							delete(values, key)
						}
						break
					}
				}
			}
		}
		delete(values, "hermes.connect.was")
		delete(values, "hermes.reconnect")
		values["hermes"+disconnectedKey] = "1"
		return stashWrite(values)
	}()
	stashMu.Unlock()
	if err != nil {
		return err
	}
	appliedMu.Lock()
	defer appliedMu.Unlock()
	raw, err := os.ReadFile(appliedPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var values map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil || values == nil {
		return ErrManagedRestore
	}
	if _, ok := values["hermes"]; !ok {
		return nil
	}
	delete(values, "hermes")
	raw, err = json.MarshalIndent(values, "", "  ")
	if err != nil {
		return err
	}
	return edit.WriteAtomic(appliedPath(), raw)
}

// hermesManagedEdit runs the editor on a private copy and replaces only the
// requested path, including when that path names a multiply linked file.
func hermesManagedEdit(path string, fn func(string) error) error {
	if _, err := managedPath(path); err != nil {
		return err
	}
	raw, exists, mode, err := hermesManagedRead(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(filepath.Dir(path), ".magpie-hermes-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	stage := filepath.Join(dir, filepath.Base(path))
	if exists {
		if err := os.WriteFile(stage, raw, os.FileMode(mode)); err != nil {
			return err
		}
	}
	if err := fn(stage); err != nil {
		return err
	}
	if _, err := managedPath(path); err != nil {
		return err
	}
	_, err = os.Stat(stage)
	if errors.Is(err, os.ErrNotExist) {
		err = os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err != nil {
		return err
	}
	defer filememo.Forget()
	return steady.Rename(stage, path)
}
func hermesScopedSetYAML(before func(string) error, path string, kvs ...edit.KV) error {
	if before == nil {
		return edit.SetYAML(path, kvs...)
	}
	if err := before(path); err != nil {
		return err
	}
	return hermesManagedEdit(path, func(p string) error { return edit.SetYAML(p, kvs...) })
}
func hermesScopedDelYAML(before func(string) error, path string, keys ...string) error {
	if before == nil {
		return edit.DelYAML(path, keys...)
	}
	if err := before(path); err != nil {
		return err
	}
	return hermesManagedEdit(path, func(p string) error { return edit.DelYAML(p, keys...) })
}
func hermesScopedSyncYAML(before func(string) error, path, key string, value func() any) error {
	if before == nil {
		return syncYAML(path, key, value)
	}
	if _, exists := edit.GetYAMLText(path, key); !exists {
		return nil
	}
	if err := before(path); err != nil {
		return err
	}
	return hermesManagedEdit(path, func(p string) error { return syncYAML(p, key, value) })
}

type hermesEnvRecord struct{ key, text string }

// hermesEnvRecords keeps quoted multiline values intact without interpreting #
// inside quotes. Invalid quoted records are rejected to keep recovery retryable.
func hermesEnvRecords(raw []byte) ([]hermesEnvRecord, error) {
	lines := strings.SplitAfter(string(raw), "\n")
	var records []hermesEnvRecord
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		match := managedHermesEnvLine.FindStringSubmatchIndex(line)
		if match == nil {
			records = append(records, hermesEnvRecord{text: line})
			continue
		}
		key := line[match[2]:match[3]]
		value := strings.TrimLeft(line[match[1]:], " \t")
		if len(value) > 0 && (value[0] == '\'' || value[0] == '"') {
			quote := value[0]
			closed := false
			escaped := false
			for {
				for j := 1; j < len(value); j++ {
					c := value[j]
					if escaped {
						escaped = false
						continue
					}
					if c == '\\' {
						escaped = true
						continue
					}
					if c == quote {
						closed = true
						break
					}
				}
				if closed {
					break
				}
				i++
				if i >= len(lines) {
					return nil, ErrManagedCapture
				}
				line += lines[i]
				value = " " + lines[i]
			}
		}
		records = append(records, hermesEnvRecord{key: key, text: line})
	}
	return records, nil
}

func hermesManagedSetEnv(path, name, value string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	records, err := hermesEnvRecords(raw)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".env-value-*")
	if err != nil {
		return err
	}
	temp := tmp.Name()
	defer os.Remove(temp)
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := edit.SetEnvFile(temp, edit.KV{Path: name, Value: value}); err != nil {
		return err
	}
	encoded, err := os.ReadFile(temp)
	if err != nil {
		return err
	}
	line := strings.TrimSuffix(string(encoded), "\n")
	var out strings.Builder
	count := 0
	for _, r := range records {
		if r.key != name {
			out.WriteString(r.text)
			continue
		}
		count++
		out.WriteString(line)
		if strings.HasSuffix(r.text, "\n") {
			out.WriteByte('\n')
		}
	}
	if count == 0 {
		if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
			out.WriteByte('\n')
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(out.String()), 0600)
}

type hermesEnvAnchorAt struct {
	ID     string `json:"id"`
	Before int    `json:"before"`
}
type hermesEnvSnapshot struct {
	Lines   []string            `json:"lines"`
	Anchors []hermesEnvAnchorAt `json:"anchors"`
}

func hermesEnvAnchor(r hermesEnvRecord) string {
	if r.key != "" {
		return "key:" + r.key
	}
	if strings.TrimSpace(r.text) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(r.text))
	return fmt.Sprintf("line:%x", sum)
}
func hermesEnvAnchorsMatch(records []hermesEnvRecord, scope string, anchors []hermesEnvAnchorAt) bool {
	wanted := map[string]hermesEnvAnchorAt{}
	seen := map[string]int{}
	for _, a := range anchors {
		if _, ok := wanted[a.ID]; ok {
			return false
		}
		wanted[a.ID] = a
	}
	before := 0
	next := 0
	for _, r := range records {
		if r.key == scope {
			before++
			continue
		}
		id := hermesEnvAnchor(r)
		a, ok := wanted[id]
		if !ok {
			continue
		}
		seen[id]++
		if seen[id] != 1 || next >= len(anchors) || anchors[next].ID != id || a.Before != before {
			return false
		}
		next++
	}
	return next == len(anchors)
}
