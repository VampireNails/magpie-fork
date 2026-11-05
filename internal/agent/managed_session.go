package agent

import "errors"

// ManagedTarget identifies one adapter-owned region. Scope distinguishes regions
// in one file (for example two env variables); Kind versions its encoding.
type ManagedTarget struct {
	Path  string `json:"path"`
	Scope string `json:"scope"`
	Kind  string `json:"kind"`
}

// ManagedSnapshot is private local recovery data, never status or diagnostics.
// Value is adapter-encoded bytes: nil, empty, and encoded null are distinct.
// Exists describes the region; FileExists and Mode describe its containing file.
type ManagedSnapshot struct {
	Exists     bool   `json:"exists"`
	FileExists bool   `json:"fileExists"`
	Mode       uint32 `json:"mode"`
	Value      []byte `json:"value"`
}

// ManagedAdapter must validate ownership and encoding, not merely a path prefix.
// Hooks run under the agent lock, must be local, and must not reenter Store or
// Session methods. Restore and Cleanup must be idempotent. Cleanup touches only
// this agent's native bookkeeping, never a rollback of whole shared files.
type ManagedAdapter struct {
	Validate func(ManagedTarget) error
	Capture  func(ManagedTarget) (ManagedSnapshot, error)
	Restore  func(ManagedTarget, ManagedSnapshot) error
	Cleanup  func() error
	// Finalize restores file existence and mode only after every region is restored.
	// It must preserve unrelated contents and be safe to repeat after a crash.
	Finalize func(ManagedFileBaseline) error
}

// ManagedFileBaseline retains the file state seen before its first region was written.
// Original keeps those exact bytes: a region restore puts the values back, but a
// YAML or JSON re-encode can still change the file's formatting, and "restored"
// has to mean the file the user had.
type ManagedFileBaseline struct {
	Path     string `json:"path"`
	Exists   bool   `json:"exists"`
	Mode     uint32 `json:"mode"`
	Original []byte `json:"original,omitempty"`
}

type ManagedPhase string

const (
	ManagedUnknown   ManagedPhase = "unknown"
	ManagedDetached  ManagedPhase = "detached"
	ManagedPrepared  ManagedPhase = "prepared"
	ManagedActive    ManagedPhase = "active"
	ManagedRestoring ManagedPhase = "restoring"
)

// ManagedStatus deliberately contains no targets, snapshots, or hook errors.
type ManagedStatus struct {
	AgentID       string       `json:"agentId"`
	Generation    uint64       `json:"generation"`
	Phase         ManagedPhase `json:"phase"`
	Pending       int          `json:"pending"`
	LastErrorCode string       `json:"lastErrorCode,omitempty"`
}

var (
	ErrManagedInvalid  = errors.New("managed: invalid target or adapter")
	ErrManagedUnknown  = errors.New("managed: journal unknown")
	ErrManagedStore    = errors.New("managed: journal storage failed")
	ErrManagedLocked   = errors.New("managed: exclusive lock unavailable")
	ErrManagedRecovery = errors.New("managed: recovery required")
	ErrManagedCapture  = errors.New("managed: capture failed")
	ErrManagedWrite    = errors.New("managed: write failed; recovery required")
	ErrManagedRestore  = errors.New("managed: restore incomplete")
	ErrManagedStale    = errors.New("managed: stale session")
)

// ManagedSession is a generation-scoped capability returned only by BeginManaged.
// A restarted process must restore the journal, then explicitly begin again.
type ManagedSession struct {
	store      *ManagedStore
	agentID    string
	generation uint64
	adapter    ManagedAdapter
}

// ManagedWrite permits captures discovered by native Set/Sync while the outer
// write already owns the lock. It is valid only inside its callback, on that
// goroutine. Every actual write must first check BeforeManagedWrite's error.
type ManagedWrite struct {
	session *ManagedSession
	journal *managedJournal
	err     error
	closed  bool
}

// NewManagedStore uses an absolute local journal directory (empty: Magpie config).
func NewManagedStore(dir string) (*ManagedStore, error) { return newManagedStore(dir) }

// BeginManaged durably captures all known initial targets before returning a
// writable session. Any existing unfinished generation requires RestoreManaged.
func (s *ManagedStore) BeginManaged(id string, a ManagedAdapter, targets ...ManagedTarget) (*ManagedSession, error) {
	if !managedAdapter(a) {
		return nil, ErrManagedInvalid
	}
	unlock, err := s.lock(id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	j, err := s.read(id)
	if err != nil {
		return nil, err
	}
	if j.Phase != ManagedDetached {
		return nil, ErrManagedRecovery
	}
	if j.Generation == ^uint64(0) {
		return nil, ErrManagedUnknown
	}
	j.Generation++
	j.Phase = ManagedPrepared
	j.CleanupDone = false
	j.LastErrorCode = ""
	if err := managedAdd(j, a, targets); err != nil {
		return nil, err
	}
	if err := s.save(j); err != nil {
		return nil, err
	}
	return &ManagedSession{store: s, agentID: id, generation: j.Generation, adapter: a}, nil
}

// BeforeManagedWrite serializes a complete local write with restore. Discoveries
// inside it use the scoped ManagedWrite, never this public locking method again.
func (s *ManagedSession) BeforeManagedWrite(write func(*ManagedWrite) error) error {
	if write == nil {
		return ErrManagedInvalid
	}
	unlock, err := s.store.lock(s.agentID)
	if err != nil {
		return err
	}
	defer unlock()
	j, err := s.store.read(s.agentID)
	if err != nil {
		return err
	}
	if j.Generation != s.generation || j.Phase == ManagedDetached {
		return ErrManagedStale
	}
	if j.Phase == ManagedRestoring || j.LastErrorCode != "" {
		return ErrManagedRecovery
	}
	j.Phase = ManagedPrepared
	if err := s.store.save(j); err != nil {
		return err
	}
	w := &ManagedWrite{session: s, journal: j}
	defer func() { w.closed = true }()
	err = write(w)
	if w.err != nil {
		err = w.err
	} else if err != nil {
		err = ErrManagedWrite
	}
	if err != nil {
		j.LastErrorCode = "write_failed"
		if saveErr := s.store.save(j); saveErr != nil {
			return saveErr
		}
		return err
	}
	j.Phase = ManagedActive
	return s.store.save(j)
}

func (w *ManagedWrite) BeforeManagedWrite(target ManagedTarget) error {
	if w.closed {
		return ErrManagedStale
	}
	if w.err != nil {
		return w.err
	}
	n := len(w.journal.Targets)
	w.err = managedAdd(w.journal, w.session.adapter, []ManagedTarget{target})
	if w.err == nil && len(w.journal.Targets) != n {
		w.err = w.session.store.save(w.journal)
	}
	return w.err
}

// RestoreManaged also handles prepared/active after a crash, without comparing
// live values. It checkpoints each restored target and retries only unfinished
// work. Native cleanup failure does not prevent attempting snapshot restoration.
func (s *ManagedStore) RestoreManaged(id string, a ManagedAdapter) error {
	if !managedAdapter(a) {
		return ErrManagedInvalid
	}
	unlock, err := s.lock(id)
	if err != nil {
		return err
	}
	defer unlock()
	j, err := s.read(id)
	if err != nil {
		return err
	}
	if j.Phase == ManagedDetached {
		return nil
	}
	for _, e := range j.Targets {
		if a.Validate(e.Target) != nil {
			return ErrManagedInvalid
		}
	}
	j.Phase = ManagedRestoring
	if err := s.save(j); err != nil {
		return err
	}
	j.LastErrorCode = ""
	if !j.CleanupDone {
		if a.Cleanup != nil && a.Cleanup() != nil {
			j.LastErrorCode = "cleanup_failed"
		} else {
			j.CleanupDone = true
		}
		if err := s.save(j); err != nil {
			return err
		}
	}
	for i := range j.Targets {
		e := &j.Targets[i]
		if e.Restored {
			continue
		}
		t, err := managedTarget(e.Target)
		if err != nil || t != e.Target || a.Validate(t) != nil {
			e.ErrorCode = "target_invalid"
		} else if a.Restore(t, *e.Snapshot) != nil {
			e.ErrorCode = "restore_failed"
		} else {
			e.Restored = true
			e.Snapshot = nil
			e.ErrorCode = ""
		}
		if e.ErrorCode != "" {
			j.LastErrorCode = "restore_failed"
		}
		if err := s.save(j); err != nil {
			return err
		}
	}
	if j.LastErrorCode != "" {
		return ErrManagedRestore
	}
	// File cleanup waits until native cleanup and all regions are restored.
	for i := range j.Files {
		f := &j.Files[i]
		if f.Finalized {
			continue
		}
		p, err := managedPath(f.Baseline.Path)
		if err != nil || p != f.Baseline.Path || a.Finalize(f.Baseline) != nil {
			f.ErrorCode = "finalize_failed"
			j.LastErrorCode = "finalize_failed"
		} else {
			f.Finalized = true
			f.ErrorCode = ""
		}
		if err := s.save(j); err != nil {
			return err
		}
	}
	if j.LastErrorCode != "" {
		return ErrManagedRestore
	}
	j.Phase = ManagedDetached
	j.Targets = nil
	j.Files = nil
	return s.save(j)
}

func (s *ManagedStore) ManagedState(id string) (ManagedStatus, error) {
	status := ManagedStatus{AgentID: id, Phase: ManagedUnknown}
	unlock, err := s.lock(id)
	if err != nil {
		return status, err
	}
	defer unlock()
	j, err := s.read(id)
	if err != nil {
		return status, err
	}
	status.Generation = j.Generation
	status.Phase = j.Phase
	status.LastErrorCode = j.LastErrorCode
	for _, e := range j.Targets {
		if !e.Restored {
			status.Pending++
		}
	}
	return status, nil
}
