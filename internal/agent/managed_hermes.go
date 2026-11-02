package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yetone/magpie/internal/edit"
	"gopkg.in/yaml.v3"
)

const managedHermesYAML = "hermes-yaml-v1"
const managedHermesEnv = "hermes-env-v1"

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
		value, ok := edit.GetEnvFile(t.Path, t.Scope)
		s.Exists = ok
		if ok {
			s.Value = []byte(value)
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
		if s.Exists {
			return edit.SetEnvFile(t.Path, edit.KV{Path: t.Scope, Value: string(s.Value)})
		}
		return edit.DelEnvFile(t.Path, t.Scope)
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
		return os.Chmod(f.Path, os.FileMode(f.Mode))
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
