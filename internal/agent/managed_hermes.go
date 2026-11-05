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
	"reflect"
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
	ParentExists    bool   `json:"parentExists"`
	ParentValue     []byte `json:"parentValue"`
	ParentAlias     string `json:"parentAlias,omitempty"`
	ParentAliasHead string `json:"parentAliasHead,omitempty"`
	ParentAliasLine string `json:"parentAliasLine,omitempty"`
	ParentAliasFoot string `json:"parentAliasFoot,omitempty"`
	ParentResolved  []byte `json:"parentResolved,omitempty"`
	Resolved        []byte `json:"resolved,omitempty"`
	ParentMap       bool   `json:"parentMap"`
	Value           []byte `json:"value"`
	Alias           string `json:"alias,omitempty"`
	AliasHead       string `json:"aliasHead,omitempty"`
	AliasLine       string `json:"aliasLine,omitempty"`
	AliasFoot       string `json:"aliasFoot,omitempty"`
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

// hermesResolved copies a subtree, resolving aliases to capture-time values.
// Reference-site comments are retained; comments and anchors from the target
// are not copied into the reference site. A recursion bound rejects cycles.
func hermesResolved(n *yaml.Node, inReference bool, depth int, remaining *int) (*yaml.Node, error) {
	if n == nil {
		return nil, nil
	}
	if depth > 100 || *remaining <= 0 {
		return nil, ErrManagedCapture
	}
	*remaining--
	if n.Kind == yaml.AliasNode {
		if n.Alias == nil {
			return nil, ErrManagedCapture
		}
		c, err := hermesResolved(n.Alias, true, depth+1, remaining)
		if err != nil {
			return nil, err
		}
		if !inReference {
			c.HeadComment, c.LineComment, c.FootComment = n.HeadComment, n.LineComment, n.FootComment
		}
		return c, nil
	}
	c := *n
	c.Alias = nil
	c.Content = nil
	if inReference {
		c.Anchor = ""
		c.HeadComment = ""
		c.LineComment = ""
		c.FootComment = ""
	}
	for _, child := range n.Content {
		v, err := hermesResolved(child, inReference, depth+1, remaining)
		if err != nil {
			return nil, err
		}
		c.Content = append(c.Content, v)
	}
	return &c, nil
}
func hermesResolvedBytes(n *yaml.Node) ([]byte, error) {
	remaining := 100000
	c, err := hermesResolved(n, false, 0, &remaining)
	if err != nil {
		return nil, err
	}
	return hermesNodeBytes(c)
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
			if parent.Kind == yaml.AliasNode {
				r.ParentAlias, r.ParentAliasHead, r.ParentAliasLine, r.ParentAliasFoot = parent.Value, parent.HeadComment, parent.LineComment, parent.FootComment
			}
			r.ParentValue, err = hermesNodeBytes(parent)
			if err != nil {
				return s, err
			}
			if parent.Kind == yaml.AliasNode {
				r.ParentResolved, err = hermesResolvedBytes(parent)
				if err != nil {
					return s, err
				}
			}
			if err != nil {
				return s, err
			}
			if parent.Kind != yaml.AliasNode {
				var check yaml.Node
				if yaml.Unmarshal(r.ParentValue, &check) != nil {
					return s, ErrManagedCapture
				}
			}
		}
	}
	n := hermesNode(parent, parts[len(parts)-1])
	s.Exists = n != nil
	if n != nil && n.Kind == yaml.AliasNode {
		r.Alias, r.AliasHead, r.AliasLine, r.AliasFoot = n.Value, n.HeadComment, n.LineComment, n.FootComment
	}
	r.Value, err = hermesNodeBytes(n)
	if err != nil {
		return s, err
	}
	// Store an independently parseable capture-time value, including nested
	// references. Alias-only older journals remain pending because they lack a value.
	if n != nil {
		r.Resolved, err = hermesResolvedBytes(n)
		if err != nil {
			return s, err
		}
		var check yaml.Node
		if err := yaml.Unmarshal(r.Resolved, &check); err != nil {
			return s, ErrManagedCapture
		}
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
	// Notes inside the region being replaced belong to the removed wiring;
	// the whole-line preservation fallback must not reattach them elsewhere.
	ownedNotes := map[string]bool{}
	if exists {
		root, err := hermesYAMLRoot(raw)
		if err != nil {
			return err
		}
		n := root
		for _, part := range strings.Split(t.Scope, ".") {
			n = hermesNode(n, part)
		}
		var collectNotes func(*yaml.Node)
		collectNotes = func(n *yaml.Node) {
			if n == nil {
				return
			}
			for _, comment := range []string{n.HeadComment, n.LineComment, n.FootComment} {
				for _, line := range strings.Split(comment, "\n") {
					if edit.IsNote(line) {
						ownedNotes[strings.TrimSpace(line)] = true
					}
				}
			}
			for _, child := range n.Content {
				collectNotes(child)
			}
		}
		collectNotes(n)
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
		} else if !ownedNotes[strings.TrimSpace(line)] {
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
						if r.ParentAlias != "" {
							return hermesMergeParentAlias(t.Path, root, p, parts[1], r)
						}
						return ErrManagedRestore
					}
				}
			}
		}
		if r.ParentAlias != "" {
			return hermesRestoreCapturedAlias(t.Path, parts[:1], hermesRegion{Alias: r.ParentAlias, AliasHead: r.ParentAliasHead, AliasLine: r.ParentAliasLine, AliasFoot: r.ParentAliasFoot, Resolved: r.ParentResolved})
		}
		return edit.SetYAML(t.Path, edit.KV{Path: parts[0], Value: edit.YAMLText(r.ParentValue)})
	}
	if s.Exists {
		if r.Alias != "" {
			return hermesRestoreCapturedAlias(t.Path, parts, r)
		}
		value := r.Value
		if len(r.Resolved) != 0 {
			value = r.Resolved
		}
		return edit.SetYAML(t.Path, edit.KV{Path: t.Scope, Value: edit.YAMLText(value)})
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

// Use the original reference only when it still resolves to the captured value.
// Otherwise restore the independent value without altering the current anchor.
func hermesRestoreCapturedAlias(path string, parts []string, r hermesRegion) error {
	// Older alias-only journals lack the original value; rebinding one could
	// silently restore a changed value, so keep it pending for recovery.
	if len(r.Resolved) == 0 {
		return ErrManagedRestore
	}
	raw, exists, _, err := hermesManagedRead(path)
	if err != nil {
		return err
	}
	if !exists {
		return ErrManagedRestore
	}
	root, err := hermesYAMLRoot(raw)
	if err != nil {
		return err
	}
	var anchor *yaml.Node
	count := 0
	var visit func(*yaml.Node)
	visit = func(n *yaml.Node) {
		if n.Anchor == r.Alias {
			anchor = n
			count++
		}
		for _, c := range n.Content {
			visit(c)
		}
	}
	visit(root)
	if count == 1 {
		var before, current any
		if yaml.Unmarshal(r.Resolved, &before) == nil && anchor.Decode(&current) == nil && reflect.DeepEqual(before, current) {
			if err := hermesRestoreAlias(path, parts, r); err == nil {
				return nil
			}
		}
	}
	if err := edit.SetYAML(path, edit.KV{Path: strings.Join(parts, "."), Value: edit.YAMLText(r.Resolved)}); err != nil {
		return err
	}
	// A managed write or an earlier restore attempt may have removed the
	// reference's line comment. Restore it at the original key, never by
	// attaching it to a neighbouring entry.
	if r.AliasLine == "" {
		return nil
	}
	raw, _, _, err = hermesManagedRead(path)
	if err != nil {
		return err
	}
	root, err = hermesYAMLRoot(raw)
	if err != nil {
		return err
	}
	parent := root
	for _, part := range parts[:len(parts)-1] {
		parent = hermesNode(parent, part)
		if parent == nil || parent.Kind != yaml.MappingNode {
			return ErrManagedRestore
		}
	}
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value != parts[len(parts)-1] {
			continue
		}
		key, value := parent.Content[i], parent.Content[i+1]
		if key.LineComment == r.AliasLine || value.LineComment == r.AliasLine {
			return nil
		}
		if key.LineComment != "" || value.LineComment != "" {
			return ErrManagedRestore
		}
		if (value.Kind == yaml.MappingNode || value.Kind == yaml.SequenceNode) && value.Style&yaml.FlowStyle == 0 {
			key.LineComment = r.AliasLine
		} else {
			value.LineComment = r.AliasLine
		}
		var out bytes.Buffer
		enc := yaml.NewEncoder(&out)
		enc.SetIndent(2)
		if err := enc.Encode(root); err != nil {
			return err
		}
		if err := enc.Close(); err != nil {
			return err
		}
		result := out.Bytes()
		if bytes.Contains(raw, []byte("\r\n")) {
			result = bytes.ReplaceAll(result, []byte("\n"), []byte("\r\n"))
		}
		if _, err := hermesYAMLRoot(result); err != nil {
			return err
		}
		return edit.WriteAtomic(path, result)
	}
	return ErrManagedRestore
}

// Restore an alias in the document that owns its anchor. Parsing the alias as
// a standalone YAML value cannot resolve its target. Missing or ambiguous
// anchors leave the journal pending instead of changing unrelated data.
func hermesRestoreAlias(path string, parts []string, r hermesRegion) error {
	raw, exists, _, err := hermesManagedRead(path)
	if err != nil {
		return err
	}
	if !exists {
		return ErrManagedRestore
	}
	root, err := hermesYAMLRoot(raw)
	if err != nil {
		return err
	}
	var anchor *yaml.Node
	count := 0
	var visit func(*yaml.Node)
	visit = func(n *yaml.Node) {
		if n.Anchor == r.Alias {
			anchor = n
			count++
		}
		for _, child := range n.Content {
			visit(child)
		}
	}
	visit(root)
	if count != 1 {
		return ErrManagedRestore
	}
	parent := root
	for _, part := range parts[:len(parts)-1] {
		parent = hermesNode(parent, part)
		if parent == nil || parent.Kind != yaml.MappingNode {
			return ErrManagedRestore
		}
	}
	key := parts[len(parts)-1]
	index := -1
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			index = i + 1
			break
		}
	}
	var contains func(*yaml.Node) bool
	contains = func(n *yaml.Node) bool {
		if n == anchor {
			return true
		}
		for _, child := range n.Content {
			if contains(child) {
				return true
			}
		}
		return false
	}
	if index >= 0 && contains(parent.Content[index]) {
		return ErrManagedRestore
	}
	lineComment := r.AliasLine
	if index >= 0 && parent.Content[index-1].LineComment == lineComment {
		parent.Content[index-1].LineComment = ""
	}
	alias := &yaml.Node{Kind: yaml.AliasNode, Value: r.Alias, Alias: anchor, HeadComment: r.AliasHead, LineComment: lineComment, FootComment: r.AliasFoot}
	if index >= 0 {
		parent.Content[index] = alias
	} else {
		parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, alias)
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	result := out.Bytes()
	if bytes.Contains(raw, []byte("\r\n")) {
		result = bytes.ReplaceAll(result, []byte("\n"), []byte("\r\n"))
	}
	if _, err := hermesYAMLRoot(result); err != nil {
		return err
	}
	return edit.WriteAtomic(path, result)
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
		if err := hermesManagedEdit(f.Path, func(p string) error { return os.Chmod(p, os.FileMode(f.Mode)) }); err != nil {
			return err
		}
		return restoreOriginalBytes(f, hermesSameDocument)
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

// Merge the captured alias value with the current siblings. The managed child
// comes from the snapshot; a present sibling is the user's current value.
func hermesMergeParentAlias(path string, root, current *yaml.Node, managed string, r hermesRegion) error {
	if len(r.ParentResolved) == 0 {
		return ErrManagedRestore
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(r.ParentResolved, &doc); err != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return ErrManagedRestore
	}
	saved := doc.Content[0]
	// Preserve the existing parent node and its comments/anchor. This also keeps
	// aliases referring to it valid when the output is re-parsed.
	merged := make([]*yaml.Node, 0, len(saved.Content)+len(current.Content))
	for i := 0; i+1 < len(saved.Content); i += 2 {
		key, value := saved.Content[i], saved.Content[i+1]
		if key.Value != managed {
			for j := 0; j+1 < len(current.Content); j += 2 {
				if current.Content[j].Value == key.Value {
					key, value = current.Content[j], current.Content[j+1]
					break
				}
			}
		}
		merged = append(merged, key, value)
	}
	for i := 0; i+1 < len(current.Content); i += 2 {
		key := current.Content[i]
		if key.Value != managed && hermesNode(saved, key.Value) == nil {
			merged = append(merged, key, current.Content[i+1])
		}
	}
	current.Content = merged
	if current.HeadComment == "" {
		current.HeadComment = r.ParentAliasHead
	}
	// A block mapping's value comment can be emitted on the following key.
	// Put the original inline alias comment on its parent key instead.
	if current.LineComment == "" && r.ParentAliasLine != "" {
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i+1] == current && root.Content[i].LineComment == "" {
				root.Content[i].LineComment = r.ParentAliasLine
				break
			}
		}
	}
	if current.FootComment == "" {
		current.FootComment = r.ParentAliasFoot
	}
	raw, _, _, err := hermesManagedRead(path)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err = enc.Encode(root); err != nil {
		return err
	}
	if err = enc.Close(); err != nil {
		return err
	}
	result := out.Bytes()
	if bytes.Contains(raw, []byte("\r\n")) {
		result = bytes.ReplaceAll(result, []byte("\n"), []byte("\r\n"))
	}
	if _, err = hermesYAMLRoot(result); err != nil {
		return err
	}
	return edit.WriteAtomic(path, result)
}
