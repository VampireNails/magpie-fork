package agent

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/steady"
)

type ManagedStore struct {
	dir  string
	wait time.Duration
}
type managedEntry struct {
	Target    ManagedTarget    `json:"target"`
	Snapshot  *ManagedSnapshot `json:"snapshot,omitempty"`
	Restored  bool             `json:"restored"`
	ErrorCode string           `json:"errorCode,omitempty"`
}
type managedFile struct {
	Baseline  ManagedFileBaseline `json:"baseline"`
	Finalized bool                `json:"finalized"`
	ErrorCode string              `json:"errorCode,omitempty"`
}
type managedJournal struct {
	Schema        int            `json:"schema"`
	Generation    uint64         `json:"generation"`
	AgentID       string         `json:"agentId"`
	Phase         ManagedPhase   `json:"phase"`
	Targets       []managedEntry `json:"targets"`
	Files         []managedFile  `json:"files"`
	CleanupDone   bool           `json:"cleanupDone"`
	LastErrorCode string         `json:"lastErrorCode,omitempty"`
}

var managedLocks sync.Map

func managedFields(raw json.RawMessage, names ...string) (map[string]json.RawMessage, bool) {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, false
	}
	m := make(map[string]json.RawMessage)
	for d.More() {
		token, err := d.Token()
		name, ok := token.(string)
		if err != nil || !ok {
			return nil, false
		}
		if _, exists := m[name]; exists {
			return nil, false
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, false
		}
		m[name] = value
	}
	if token, err = d.Token(); err != nil || token != json.Delim('}') {
		return nil, false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, false
	}
	for _, name := range names {
		v, ok := m[name]
		if !ok || name != "value" && string(v) == "null" {
			return nil, false
		}
	}
	return m, true
}

func managedAllowed(fields map[string]json.RawMessage, names ...string) bool {
	for name, value := range fields {
		found := false
		for _, allowed := range names {
			if name == allowed {
				found = true
				break
			}
		}
		if !found || name != "value" && string(value) == "null" {
			return false
		}
	}
	return true
}

func managedShape(fields map[string]json.RawMessage) bool {
	var entries []json.RawMessage
	if json.Unmarshal(fields["targets"], &entries) != nil || entries == nil {
		return false
	}
	for _, raw := range entries {
		e, ok := managedFields(raw, "target", "restored")
		if !ok || !managedAllowed(e, "target", "snapshot", "restored", "errorCode") {
			return false
		}
		if t, ok := managedFields(e["target"], "path", "scope", "kind"); !ok || !managedAllowed(t, "path", "scope", "kind") {
			return false
		}
		if snapshot, exists := e["snapshot"]; exists {
			s, ok := managedFields(snapshot, "exists", "fileExists", "mode", "value")
			if !ok || !managedAllowed(s, "exists", "fileExists", "mode", "value") || string(s["value"]) != "null" && (len(s["value"]) == 0 || s["value"][0] != '"') {
				return false
			}
		}
	}
	var files []json.RawMessage
	if json.Unmarshal(fields["files"], &files) != nil || files == nil {
		return false
	}
	for _, raw := range files {
		f, ok := managedFields(raw, "baseline", "finalized")
		if !ok || !managedAllowed(f, "baseline", "finalized", "errorCode") {
			return false
		}
		if b, ok := managedFields(f["baseline"], "path", "exists", "mode"); !ok || !managedAllowed(b, "path", "exists", "mode", "original") {
			return false
		}
	}
	return true
}

func newManagedStore(dir string) (*ManagedStore, error) {
	if dir == "" {
		dir = filepath.Join(appdir.Config(), "remote-agent-sessions")
	}
	if !filepath.IsAbs(dir) {
		return nil, ErrManagedInvalid
	}
	return &ManagedStore{dir: filepath.Clean(dir), wait: filelock.Wait}, nil
}
func managedID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
func managedPath(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", ErrManagedInvalid
	}
	p = filepath.Clean(p)
	for part := p; ; part = filepath.Dir(part) {
		fi, err := os.Lstat(part)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", ErrManagedInvalid
		}
		if err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return "", ErrManagedInvalid
		}
		if filepath.Dir(part) == part {
			break
		}
	}
	return p, nil
}
func (s *ManagedStore) path(id string) string { return filepath.Join(s.dir, id+".json") }
func (s *ManagedStore) lock(id string) (func(), error) {
	if !managedID(id) {
		return nil, ErrManagedInvalid
	}
	if _, err := managedPath(s.dir); err != nil {
		return nil, ErrManagedStore
	}
	lockKey := s.path(id)
	if runtime.GOOS == "windows" {
		lockKey = strings.ToLower(lockKey)
	}
	m, _ := managedLocks.LoadOrStore(lockKey, new(sync.Mutex))
	mu := m.(*sync.Mutex)
	mu.Lock()
	if os.MkdirAll(s.dir, 0700) != nil {
		mu.Unlock()
		return nil, ErrManagedStore
	}
	unlock, held := filelock.Lock(s.path(id)+".lock", s.wait)
	if !held {
		mu.Unlock()
		return nil, ErrManagedLocked
	}
	return func() { unlock(); mu.Unlock() }, nil
}
func managedTarget(t ManagedTarget) (ManagedTarget, error) {
	p, err := managedPath(t.Path)
	if err != nil || t.Scope == "" || t.Kind == "" || strings.ContainsRune(t.Scope, 0) || len(t.Scope) > 1024 || len(t.Kind) > 128 {
		return ManagedTarget{}, ErrManagedInvalid
	}
	t.Path = p
	return t, nil
}
func managedFileKey(path string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}
func managedKey(t ManagedTarget) string {
	p := t.Path
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p + "\x00" + t.Scope
}
func (s *ManagedStore) read(id string) (*managedJournal, error) {
	b, err := steady.ReadFile(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return &managedJournal{Schema: 1, AgentID: id, Phase: ManagedDetached}, nil
	}
	if err != nil {
		return nil, ErrManagedUnknown
	}
	// A parseable truncation must not look like an empty valid journal.
	fields, ok := managedFields(b, "schema", "generation", "agentId", "phase", "targets", "files", "cleanupDone")
	if !ok || !managedAllowed(fields, "schema", "generation", "agentId", "phase", "targets", "files", "cleanupDone", "lastErrorCode") || !managedShape(fields) {
		return nil, ErrManagedUnknown
	}
	j := new(managedJournal)
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if d.Decode(j) != nil {
		return nil, ErrManagedUnknown
	}
	var extra any
	if d.Decode(&extra) != io.EOF || j.Schema != 1 || j.AgentID != id || j.Generation == 0 {
		return nil, ErrManagedUnknown
	}
	if j.LastErrorCode != "" && j.LastErrorCode != "write_failed" && j.LastErrorCode != "cleanup_failed" && j.LastErrorCode != "restore_failed" && j.LastErrorCode != "finalize_failed" {
		return nil, ErrManagedUnknown
	}
	switch j.Phase {
	case ManagedPrepared, ManagedActive:
		if j.CleanupDone {
			return nil, ErrManagedUnknown
		}
	case ManagedRestoring:
	case ManagedDetached:
		if len(j.Targets) != 0 || len(j.Files) != 0 {
			return nil, ErrManagedUnknown
		}
	default:
		return nil, ErrManagedUnknown
	}
	seen := map[string]bool{}
	files := map[string]bool{}
	matchedFiles := map[string]bool{}
	for _, f := range j.Files {
		p, err := managedPath(f.Baseline.Path)
		key := managedFileKey(p)
		if err != nil || p != f.Baseline.Path || files[key] || f.Baseline.Mode & ^uint32(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || f.Finalized && j.Phase != ManagedRestoring || f.ErrorCode != "" && f.ErrorCode != "finalize_failed" {
			return nil, ErrManagedUnknown
		}
		files[key] = true
	}
	for _, e := range j.Targets {
		if e.ErrorCode != "" && e.ErrorCode != "target_invalid" && e.ErrorCode != "restore_failed" {
			return nil, ErrManagedUnknown
		}
		t, err := managedTarget(e.Target)
		key := managedKey(t)
		if err != nil || t != e.Target || seen[key] || !files[managedFileKey(t.Path)] || e.Restored != (e.Snapshot == nil) || e.Restored && j.Phase != ManagedRestoring {
			return nil, ErrManagedUnknown
		}
		if e.Snapshot != nil && (e.Snapshot.Mode & ^uint32(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || e.Snapshot.Exists && !e.Snapshot.FileExists) {
			return nil, ErrManagedUnknown
		}
		seen[key] = true
		matchedFiles[managedFileKey(t.Path)] = true
	}
	if len(files) != len(matchedFiles) {
		return nil, ErrManagedUnknown
	}
	for _, f := range j.Files {
		if !f.Finalized {
			continue
		}
		if !j.CleanupDone {
			return nil, ErrManagedUnknown
		}
		for _, e := range j.Targets {
			if !e.Restored {
				return nil, ErrManagedUnknown
			}
		}
	}
	return j, nil
}
func (s *ManagedStore) save(j *managedJournal) error {
	copy := *j
	if copy.Targets == nil {
		copy.Targets = []managedEntry{}
	}
	if copy.Files == nil {
		copy.Files = []managedFile{}
	}
	b, err := json.Marshal(&copy)
	if err != nil {
		return ErrManagedStore
	}
	if steady.WriteFile(s.path(j.AgentID), b, 0600) != nil {
		return ErrManagedStore
	}
	return nil
}
func managedAdapter(a ManagedAdapter) bool {
	return a.Validate != nil && a.Capture != nil && a.Restore != nil && a.Finalize != nil
}
func managedAdd(j *managedJournal, a ManagedAdapter, targets []ManagedTarget) error {
	for _, target := range targets {
		t, err := managedTarget(target)
		if err != nil || a.Validate(t) != nil {
			return ErrManagedInvalid
		}
		found := false
		for _, e := range j.Targets {
			if managedKey(e.Target) == managedKey(t) {
				if e.Target.Kind != t.Kind {
					return ErrManagedInvalid
				}
				found = true
				break
			}
		}
		if found {
			continue
		}
		snap, err := a.Capture(t)
		if err != nil || snap.Mode & ^uint32(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || snap.Exists && !snap.FileExists {
			return ErrManagedCapture
		}
		if snap.Value != nil {
			snap.Value = append([]byte{}, snap.Value...)
		}
		fileSeen := false
		for _, f := range j.Files {
			if managedFileKey(f.Baseline.Path) == managedFileKey(t.Path) {
				fileSeen = true
				break
			}
		}
		if !fileSeen {
			j.Files = append(j.Files, managedFile{Baseline: ManagedFileBaseline{
				Path:     t.Path,
				Exists:   snap.FileExists,
				Mode:     snap.Mode,
				Original: originalBytes(t.Path, snap.FileExists),
			}})
		}
		j.Targets = append(j.Targets, managedEntry{Target: t, Snapshot: &snap})
	}
	return nil
}
