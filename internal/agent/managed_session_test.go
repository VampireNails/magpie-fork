package agent

import (
	"bufio"
	"time"

	"encoding/json"
	"errors"
	"fmt"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/steady"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// This adapter is a transaction fixture, not evidence of real agent support.
func managedFixture(t *testing.T) (*ManagedStore, ManagedAdapter, ManagedTarget) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewManagedStore(filepath.Join(dir, "journals"))
	if err != nil {
		t.Fatal(err)
	}
	target := ManagedTarget{Path: filepath.Join(dir, "config"), Scope: "file", Kind: "bytes-v1"}
	a := ManagedAdapter{
		Finalize: func(ManagedFileBaseline) error { return nil },
		Validate: func(v ManagedTarget) error {
			if filepath.Dir(v.Path) != dir || v.Scope != "file" || v.Kind != "bytes-v1" {
				return errors.New("not owned")
			}
			return nil
		},
		Capture: func(v ManagedTarget) (ManagedSnapshot, error) {
			fi, err := os.Stat(v.Path)
			if errors.Is(err, os.ErrNotExist) {
				return ManagedSnapshot{}, nil
			}
			if err != nil {
				return ManagedSnapshot{}, err
			}
			b, err := os.ReadFile(v.Path)
			return ManagedSnapshot{Exists: true, FileExists: true, Mode: uint32(fi.Mode().Perm()), Value: b}, err
		},
		Restore: func(v ManagedTarget, b ManagedSnapshot) error {
			if !b.FileExists {
				err := os.Remove(v.Path)
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			if err := os.WriteFile(v.Path, b.Value, os.FileMode(b.Mode)); err != nil {
				return err
			}
			return os.Chmod(v.Path, os.FileMode(b.Mode))
		},
	}
	return s, a, target
}

func managedPut(t *testing.T, p, stringValue string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(stringValue), 0600); err != nil {
		t.Fatal(err)
	}
}
func managedRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func managedBegin(t *testing.T, s *ManagedStore, a ManagedAdapter, targets ...ManagedTarget) *ManagedSession {
	t.Helper()
	session, err := s.BeginManaged("alpha", a, targets...)
	if err != nil {
		t.Fatal(err)
	}
	return session
}
func managedChange(t *testing.T, session *ManagedSession, target ManagedTarget, value string) {
	t.Helper()
	if err := session.BeforeManagedWrite(func(w *ManagedWrite) error {
		if err := w.BeforeManagedWrite(target); err != nil {
			return err
		}
		return os.WriteFile(target.Path, []byte(value), 0600)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestManagedPreparedPersistedBeforeAnyWrite(t *testing.T) {
	s, a, target := managedFixture(t)
	managedPut(t, target.Path, "baseline")
	session := managedBegin(t, s, a, target)
	status, err := s.ManagedState("alpha")
	if err != nil || status.Phase != ManagedPrepared || status.Pending != 1 {
		t.Fatalf("prepare not durable: phase=%s error=%v", status.Phase, err)
	}
	// Reopen from disk, not an in-memory status cache.
	reopened, err := NewManagedStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	status, err = reopened.ManagedState("alpha")
	if err != nil || status.Phase != ManagedPrepared {
		t.Fatal("reopened journal missing prepare")
	}
	managedChange(t, session, target, "managed")
	if err := reopened.RestoreManaged("alpha", a); err != nil {
		t.Fatal(err)
	}
	if managedRead(t, target.Path) != "baseline" {
		t.Fatal("baseline not restored")
	}
}

func TestManagedCaptureFailureDoesNotAuthorizeWrite(t *testing.T) {
	s, a, target := managedFixture(t)
	canary := "CANARY-managed-secret-928"
	a.Capture = func(ManagedTarget) (ManagedSnapshot, error) { return ManagedSnapshot{}, errors.New(canary) }
	if _, err := s.BeginManaged("alpha", a, target); !errors.Is(err, ErrManagedCapture) || strings.Contains(fmt.Sprint(err), canary) {
		t.Fatal("capture error missing or leaked secret")
	}
	session := managedBegin(t, s, a)
	writes := 0
	err := session.BeforeManagedWrite(func(w *ManagedWrite) error {
		if err := w.BeforeManagedWrite(target); err != nil {
			return err
		}
		writes++
		return nil
	})
	if !errors.Is(err, ErrManagedCapture) || writes != 0 {
		t.Fatal("capture failure allowed a write")
	}
}

func TestManagedSameTargetKeepsBaselineAndNewTargetAppends(t *testing.T) {
	s, a, target := managedFixture(t)
	managedPut(t, target.Path, "B0")
	captures := 0
	capture := a.Capture
	a.Capture = func(v ManagedTarget) (ManagedSnapshot, error) { captures++; return capture(v) }
	session := managedBegin(t, s, a, target)
	managedChange(t, session, target, "model-one")
	managedChange(t, session, target, "model-two")
	newTarget := target
	newTarget.Path = filepath.Join(filepath.Dir(target.Path), "new-profile")
	managedChange(t, session, newTarget, "new-model")
	if captures != 2 {
		t.Fatalf("capture count=%d, want once per target", captures)
	}
	if err := s.RestoreManaged("alpha", a); err != nil {
		t.Fatal(err)
	}
	if managedRead(t, target.Path) != "B0" {
		t.Fatal("baseline replaced on switch")
	}
	if _, err := os.Stat(newTarget.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing target not removed")
	}
}

func TestManagedScopedCaptureInsideWriteAndLateLeaseRejected(t *testing.T) {
	s, a, target := managedFixture(t)
	managedPut(t, target.Path, "B0")
	session := managedBegin(t, s, a, target)
	var expired *ManagedWrite
	newTarget := target
	newTarget.Path = filepath.Join(filepath.Dir(target.Path), "discovered.env")
	if err := session.BeforeManagedWrite(func(w *ManagedWrite) error {
		expired = w
		if err := w.BeforeManagedWrite(target); err != nil {
			return err
		}
		// Native Set/Sync discovers a target while the public lock is held.
		if err := w.BeforeManagedWrite(newTarget); err != nil {
			return err
		}
		return os.WriteFile(newTarget.Path, []byte("new-value"), 0600)
	}); err != nil {
		t.Fatal(err)
	}
	if err := expired.BeforeManagedWrite(target); !errors.Is(err, ErrManagedStale) {
		t.Fatal("expired scoped lease accepted")
	}
	if err := s.RestoreManaged("alpha", a); err != nil {
		t.Fatal(err)
	}
}

func TestManagedMissingNullEmptyAndModeRoundTrip(t *testing.T) {
	for _, value := range []struct {
		name   string
		exists bool
		data   []byte
	}{{"missing", false, nil}, {"null", true, []byte("null")}, {"empty", true, []byte{}}, {"nil", true, nil}} {
		t.Run(value.name, func(t *testing.T) {
			s, a, target := managedFixture(t)
			original := ManagedSnapshot{Exists: value.exists, FileExists: true, Mode: 0640, Value: value.data}
			a.Capture = func(ManagedTarget) (ManagedSnapshot, error) { return original, nil }
			var restored *ManagedSnapshot
			a.Restore = func(_ ManagedTarget, snap ManagedSnapshot) error { restored = &snap; return nil }
			managedBegin(t, s, a, target)
			if err := s.RestoreManaged("alpha", a); err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(original)
			c, _ := json.Marshal(restored)
			if string(b) != string(c) {
				t.Fatal("snapshot lost presence, bytes or mode")
			}
		})
	}
}

func TestManagedActiveCrashRequiresRecoveryAndStaleHandleCannotWrite(t *testing.T) {
	s, a, target := managedFixture(t)
	managedPut(t, target.Path, "B0")
	session := managedBegin(t, s, a, target)
	managedChange(t, session, target, "managed")
	restarted, _ := NewManagedStore(s.dir)
	if _, err := restarted.BeginManaged("alpha", a, target); !errors.Is(err, ErrManagedRecovery) {
		t.Fatal("active crash allowed reconnect")
	}
	if err := restarted.RestoreManaged("alpha", a); err != nil {
		t.Fatal(err)
	}
	if managedRead(t, target.Path) != "B0" {
		t.Fatal("active crash did not recover")
	}
	next := managedBegin(t, restarted, a, target)
	if next.generation == session.generation {
		t.Fatal("generation reused")
	}
	writes := 0
	err := session.BeforeManagedWrite(func(*ManagedWrite) error { writes++; return nil })
	if !errors.Is(err, ErrManagedStale) || writes != 0 {
		t.Fatal("stale session wrote")
	}
}

func TestManagedPartialRestoreRetriesOnlyRemainingAndCleanup(t *testing.T) {
	s, a, target := managedFixture(t)
	managedPut(t, target.Path, "B0")
	second := target
	second.Path = filepath.Join(filepath.Dir(target.Path), "second")
	managedPut(t, second.Path, "second-B0")
	session := managedBegin(t, s, a, target, second)
	managedChange(t, session, target, "changed")
	managedChange(t, session, second, "changed")
	restore := a.Restore
	calls := map[string]int{}
	fail := true
	cleanupCalls := 0
	a.Cleanup = func() error {
		cleanupCalls++
		if fail {
			return errors.New("CANARY-cleanup")
		}
		return nil
	}
	a.Restore = func(v ManagedTarget, b ManagedSnapshot) error {
		calls[v.Path]++
		if v.Path == second.Path && fail {
			return errors.New("CANARY-restore")
		}
		return restore(v, b)
	}
	if err := s.RestoreManaged("alpha", a); !errors.Is(err, ErrManagedRestore) || strings.Contains(fmt.Sprint(err), "CANARY") {
		t.Fatal("partial restore not reported safely")
	}
	status, err := s.ManagedState("alpha")
	if err != nil || status.Phase != ManagedRestoring || status.Pending != 1 {
		t.Fatal("partial state lost")
	}
	fail = false
	if err := s.RestoreManaged("alpha", a); err != nil {
		t.Fatal(err)
	}
	if calls[target.Path] != 1 || calls[second.Path] != 2 || cleanupCalls != 2 {
		t.Fatal("retry replayed completed work or skipped pending work")
	}
	if err := s.RestoreManaged("alpha", a); err != nil {
		t.Fatal(err)
	}
	if calls[target.Path] != 1 || calls[second.Path] != 2 || cleanupCalls != 2 {
		t.Fatal("duplicate exit rewrote state")
	}
}

func TestManagedConcurrentWritersSerializeAndAgentsRemainIndependent(t *testing.T) {
	s, a, target := managedFixture(t)
	managedPut(t, target.Path, "B0")
	session := managedBegin(t, s, a, target)
	var wg sync.WaitGroup
	var mu sync.Mutex
	inside := 0
	max := 0
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- session.BeforeManagedWrite(func(w *ManagedWrite) error {
				if err := w.BeforeManagedWrite(target); err != nil {
					return err
				}
				mu.Lock()
				inside++
				if inside > max {
					max = inside
				}
				mu.Unlock()
				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if max != 1 {
		t.Fatal("same agent writes overlapped")
	}
	beta := target
	beta.Path = filepath.Join(filepath.Dir(target.Path), "beta")
	managedPut(t, beta.Path, "beta-B0")
	b, err := s.BeginManaged("beta", a, beta)
	if err != nil {
		t.Fatal(err)
	}
	managedChange(t, b, beta, "beta-managed")
	if err := s.RestoreManaged("alpha", a); err != nil {
		t.Fatal(err)
	}
	state, err := s.ManagedState("beta")
	if err != nil || state.Phase != ManagedActive || managedRead(t, beta.Path) != "beta-managed" {
		t.Fatal("alpha restore touched beta")
	}
}

func TestManagedDamagedJournalIsUnknown(t *testing.T) {
	s, a, target := managedFixture(t)
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path("alpha"), []byte(`{"schema":1`), 0600); err != nil {
		t.Fatal(err)
	}
	if state, err := s.ManagedState("alpha"); !errors.Is(err, ErrManagedUnknown) || state.Phase != ManagedUnknown {
		t.Fatal("corruption treated as empty")
	}
	if _, err := s.BeginManaged("alpha", a, target); !errors.Is(err, ErrManagedUnknown) {
		t.Fatal("corruption overwritten")
	}
	if err := s.RestoreManaged("alpha", a); !errors.Is(err, ErrManagedUnknown) {
		t.Fatal("corruption reported restored")
	}
}

func TestManagedRejectsForeignTargetAndKeepsJournal(t *testing.T) {
	s, a, target := managedFixture(t)
	session := managedBegin(t, s, a, target)
	foreign := target
	foreign.Path = filepath.Join(t.TempDir(), "foreign")
	wrote := false
	err := session.BeforeManagedWrite(func(w *ManagedWrite) error {
		if err := w.BeforeManagedWrite(foreign); err != nil {
			return err
		}
		wrote = true
		return nil
	})
	if !errors.Is(err, ErrManagedInvalid) || wrote {
		t.Fatal("foreign write authorized")
	}
	if err := s.RestoreManaged("alpha", a); err != nil {
		t.Fatal(err)
	}
}

// The child holds the same OS lock until its stdin closes.
func TestManagedLockChild(t *testing.T) {
	p := os.Getenv("MAGPIE_MANAGED_LOCK_TEST")
	if p == "" {
		return
	}
	unlock, held := filelock.Lock(p, time.Second)
	if !held {
		t.Fatal("child could not take lock")
	}
	defer unlock()
	fmt.Fprintln(os.Stdout, "locked")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}
func TestManagedCrossProcessLockFailsClosed(t *testing.T) {
	s, a, target := managedFixture(t)
	managedPut(t, target.Path, "baseline")
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := proc.Command(exe, "-test.run=^TestManagedLockChild$")
	cmd.Env = append(os.Environ(), "MAGPIE_MANAGED_LOCK_TEST="+s.path("alpha")+".lock")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		in.Close()
		if cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		t.Fatal("child did not lock")
	}
	s.wait = 20 * time.Millisecond
	if _, err := s.BeginManaged("alpha", a, target); !errors.Is(err, ErrManagedLocked) {
		t.Fatal("write allowed without lock")
	}
	if _, err := os.Stat(s.path("alpha")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("journal written without lock")
	}
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	// Avoid a second Wait in deferred cleanup.
	cmd.Process = nil
}
func TestManagedUntrustedErrorCodeCannotLeak(t *testing.T) {
	s, a, target := managedFixture(t)
	managedBegin(t, s, a, target)
	b, err := os.ReadFile(s.path("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	b = []byte(strings.Replace(string(b), `"phase":"prepared"`, `"lastErrorCode":"CANARY-private","phase":"prepared"`, 1))
	if err := os.WriteFile(s.path("alpha"), b, 0600); err != nil {
		t.Fatal(err)
	}
	state, err := s.ManagedState("alpha")
	if !errors.Is(err, ErrManagedUnknown) || strings.Contains(fmt.Sprint(state), "CANARY-private") {
		t.Fatal("untrusted status leaked")
	}
}

func TestManagedIncompleteJournalIsUnknown(t *testing.T) {
	s, a, target := managedFixture(t)
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		t.Fatal(err)
	}
	// A parseable but truncated journal cannot prove that no snapshots exist.
	data := []byte(`{"schema":1,"generation":1,"agentId":"alpha","phase":"active"}`)
	if err := os.WriteFile(s.path("alpha"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreManaged("alpha", a); !errors.Is(err, ErrManagedUnknown) {
		t.Fatal("incomplete journal treated as recoverable")
	}
	if _, err := s.BeginManaged("alpha", a, target); !errors.Is(err, ErrManagedUnknown) {
		t.Fatal("incomplete journal overwritten")
	}
}

func TestManagedFileBaselineAcrossRegionsAndRetry(t *testing.T) {
	for _, tc := range []struct {
		name, initial, external string
		exists                  bool
	}{
		{"originally missing", "", "", false}, {"existing empty", "", "", true}, {"unrelated content", "", "user=keep\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, a, target := managedFixture(t)
			target.Scope = "A"
			target.Kind = "env"
			if tc.exists {
				managedPut(t, target.Path, tc.initial)
			}
			values := map[string]string{}
			exists := map[string]bool{}
			a.Validate = func(v ManagedTarget) error {
				if v.Path != target.Path || v.Kind != "env" {
					return errors.New("foreign")
				}
				return nil
			}
			a.Capture = func(v ManagedTarget) (ManagedSnapshot, error) {
				_, err := os.Stat(v.Path)
				file := err == nil
				return ManagedSnapshot{Exists: exists[v.Scope], FileExists: file, Mode: 0600, Value: []byte(values[v.Scope])}, nil
			}
			fail := true
			a.Restore = func(v ManagedTarget, b ManagedSnapshot) error {
				if v.Scope == "B" && fail {
					return errors.New("retry")
				}
				delete(values, v.Scope)
				delete(exists, v.Scope)
				if v.Scope == "A" {
					return os.WriteFile(v.Path, nil, 0600)
				}
				return nil
			}
			finalCalls := 0
			a.Finalize = func(b ManagedFileBaseline) error {
				finalCalls++
				if b.Exists != tc.exists {
					t.Error("file baseline changed")
				}
				if !b.Exists && tc.external == "" {
					err := os.Remove(b.Path)
					if errors.Is(err, os.ErrNotExist) {
						return nil
					}
					return err
				}
				return nil
			}
			session := managedBegin(t, s, a, target)
			managedChange(t, session, target, "managed-A")
			b := target
			b.Scope = "B"
			if err := session.BeforeManagedWrite(func(w *ManagedWrite) error {
				if err := w.BeforeManagedWrite(b); err != nil {
					return err
				}
				values["B"] = "managed"
				exists["B"] = true
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.RestoreManaged("alpha", a); !errors.Is(err, ErrManagedRestore) {
				t.Fatal("partial failure lost")
			}
			if finalCalls != 0 {
				t.Fatal("file finalized while regions remain")
			}
			if tc.external != "" {
				managedPut(t, target.Path, tc.external)
			}
			fail = false
			if err := s.RestoreManaged("alpha", a); err != nil {
				t.Fatal(err)
			}
			if finalCalls != 1 {
				t.Fatal("file finalization not performed")
			}
			data, err := os.ReadFile(target.Path)
			if !tc.exists && tc.external == "" {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatal("created file left behind")
				}
			} else if err != nil || string(data) != tc.external {
				t.Fatal("unrelated content or existing file lost")
			}
		})
	}
}

func TestManagedFinalizationFailureRetainsBaselineForRetry(t *testing.T) {
	s, a, target := managedFixture(t)
	session := managedBegin(t, s, a, target)
	managedChange(t, session, target, "managed")
	finalize := a.Finalize
	attempt := 0
	a.Finalize = func(b ManagedFileBaseline) error {
		attempt++
		if b.Exists {
			t.Error("missing baseline lost")
		}
		if attempt == 1 {
			return errors.New("CANARY-secret")
		}
		return finalize(b)
	}
	if err := s.RestoreManaged("alpha", a); !errors.Is(err, ErrManagedRestore) || strings.Contains(fmt.Sprint(err), "CANARY") {
		t.Fatal("finalization failure hidden or leaked")
	}
	status, err := s.ManagedState("alpha")
	if err != nil || status.Phase != ManagedRestoring || status.LastErrorCode != "finalize_failed" {
		t.Fatal("finalization failure not retained")
	}
	if err := s.RestoreManaged("alpha", a); err != nil || attempt != 2 {
		t.Fatal("finalization not retried")
	}
}

func TestManagedJournalFlushFailureStopsWrites(t *testing.T) {
	s, a, target := managedFixture(t)
	managedPut(t, target.Path, "baseline")
	old := steady.Sync
	steady.Sync = func(*os.File) error { return errors.New("CANARY-flush") }
	defer func() { steady.Sync = old }()
	_, err := s.BeginManaged("alpha", a, target)
	if !errors.Is(err, ErrManagedStore) || strings.Contains(fmt.Sprint(err), "CANARY") {
		t.Fatal("failed flush allowed prepare or leaked")
	}
	if managedRead(t, target.Path) != "baseline" {
		t.Fatal("capture failure changed agent file")
	}
	if _, err := os.Stat(s.path("alpha")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed flush published journal")
	}
}
func TestManagedMissingSnapshotFieldsIsUnknown(t *testing.T) {
	s, a, target := managedFixture(t)
	managedBegin(t, s, a, target)
	b, err := os.ReadFile(s.path("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	b = []byte(strings.Replace(string(b), `"fileExists":false,`, "", 1))
	if err := os.WriteFile(s.path("alpha"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreManaged("alpha", a); !errors.Is(err, ErrManagedUnknown) {
		t.Fatal("incomplete snapshot accepted")
	}
}

func TestManagedNullBooleanSnapshotIsUnknown(t *testing.T) {
	s, a, target := managedFixture(t)
	managedBegin(t, s, a, target)
	b, err := os.ReadFile(s.path("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	b = []byte(strings.Replace(string(b), `"fileExists":false`, `"fileExists":null`, 1))
	if err := os.WriteFile(s.path("alpha"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreManaged("alpha", a); !errors.Is(err, ErrManagedUnknown) {
		t.Fatal("null boolean accepted")
	}
}
