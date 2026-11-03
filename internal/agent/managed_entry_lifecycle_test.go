package agent

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/appdir"
)

type entryLifecycleFixture struct {
	a          *Agent
	store      *ManagedStore
	m          *ManagedEntries
	key, value string
	baseline   []byte
}

func entryLifecycleSetup(t *testing.T, id string) entryLifecycleFixture {
	t.Helper()
	home := managedEntryHome(t)
	managedEntryConfigured(t)
	f := entryLifecycleFixture{a: workbuddy(home), key: "provider", value: "magpie", baseline: []byte("[]\n")}
	if id == "hermes" {
		f.a, f.key, f.value = hermes(home), "model", "magpie/fixture/one"
		f.baseline = []byte("model:\n  default: own\n  provider: own\n")
	}
	if err := os.MkdirAll(filepath.Dir(f.a.Path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.a.Path, f.baseline, 0600); err != nil {
		t.Fatal(err)
	}
	var err error
	f.store, err = NewManagedStore("")
	if err != nil {
		t.Fatal(err)
	}
	f.m = entryLifecycleEnable(t, f.store)
	t.Cleanup(func() {
		if v, ok := managedOwners.Load(appdir.Config()); ok {
			_ = v.(*ManagedEntries).Close()
		}
	})
	if err := f.a.Apply(f.key, f.value); err != nil {
		t.Fatal(err)
	}
	return f
}

func entryLifecycleEnable(t *testing.T, store *ManagedStore) *ManagedEntries {
	t.Helper()
	m, err := EnableManagedEntries(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Recover(); err != nil {
		t.Fatal(err)
	}
	m.SetConnection(true, true)
	return m
}

type entryLifecycleFile struct {
	Bytes string
	Mode  fs.FileMode
}

// Include existence, all adapter files and the raw journal, not just status.
func entryLifecycleSnapshot(t *testing.T, f entryLifecycleFixture) map[string]entryLifecycleFile {
	t.Helper()
	files := map[string]entryLifecycleFile{}
	err := filepath.WalkDir(filepath.Dir(f.a.Path), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		var b []byte
		if !d.IsDir() {
			b, err = os.ReadFile(p)
			if err != nil {
				return err
			}
		}
		files[p] = entryLifecycleFile{string(b), info.Mode()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	p := f.store.path(f.a.ID)
	files[p] = entryLifecycleFile{Bytes: string(managedEntryBytes(t, p))}
	return files
}

func entryLifecycleBaseline(t *testing.T, f entryLifecycleFixture) {
	t.Helper()
	b := managedEntryBytes(t, f.a.Path)
	if f.a.ID == "workbuddy" {
		if !bytes.Equal(b, f.baseline) {
			t.Error("normal disconnect did not restore WorkBuddy B0")
		}
	} else if !bytes.Contains(b, []byte("default: own")) || !bytes.Contains(b, []byte("provider: own")) || bytes.Contains(b, []byte("magpie:")) {
		t.Error("normal disconnect did not restore Hermes B0 model region")
	}
	state, err := f.store.ManagedState(f.a.ID)
	if err != nil || state.Phase != ManagedDetached {
		t.Fatalf("disconnect state: %+v, %v", state, err)
	}
}

func TestManagedEntryDelayedDisconnectCannotRestoreNewOwnerGeneration(t *testing.T) {
	for _, id := range []string{"workbuddy", "hermes"} {
		t.Run(id, func(t *testing.T) {
			f := entryLifecycleSetup(t, id)
			oldState, err := f.store.ManagedState(id)
			if err != nil {
				t.Fatal(err)
			}
			loaded, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			var once sync.Once
			resume := func() { once.Do(func() { close(release) }) }
			received := false
			defer func() {
				resume()
				if !received {
					<-done
				}
			}()
			// Installed before starting the call and never changed afterward.
			f.m.disconnectLoaded = func() { close(loaded); <-release }
			go func() { done <- f.a.Disconnect() }()
			<-loaded // actual public Disconnect has loaded the old controller
			if err := f.m.Close(); err != nil {
				t.Fatal(err)
			}
			store, err := NewManagedStore(f.store.dir)
			if err != nil {
				t.Fatal(err)
			}
			n := entryLifecycleEnable(t, store)
			if err := f.a.Apply(f.key, f.value); err != nil {
				t.Fatal(err)
			}
			newState, err := store.ManagedState(id)
			if err != nil {
				t.Fatal(err)
			}
			if newState.Generation == oldState.Generation {
				t.Fatal("fixture did not start a new generation")
			}
			before := entryLifecycleSnapshot(t, f)
			resume()
			err = <-done
			received = true
			if !errors.Is(err, ErrManagedStale) {
				t.Errorf("old Disconnect = %v; want stale controller", err)
			}
			afterDisconnect := entryLifecycleSnapshot(t, f)
			if !reflect.DeepEqual(before, afterDisconnect) {
				t.Error("old Disconnect changed new owner files or journal")
			}
			after, err := store.ManagedState(id)
			if err != nil || after.Generation != newState.Generation || after.Phase != newState.Phase {
				t.Errorf("new generation changed: %+v, %v", after, err)
			}
			if err := f.m.Close(); !errors.Is(err, ErrManagedStale) {
				t.Errorf("repeated old Close = %v", err)
			}
			if !reflect.DeepEqual(afterDisconnect, entryLifecycleSnapshot(t, f)) {
				t.Error("repeated old Close changed new generation")
			}
			if v, ok := managedOwners.Load(appdir.Config()); !ok || v != n {
				t.Error("new controller was unregistered")
			}
			if err := f.a.Disconnect(); err != nil {
				t.Fatal(err)
			}
			entryLifecycleBaseline(t, f)
		})
	}
}

func TestManagedEntryLifecycleRestoreFailureKeepsControllerForRetry(t *testing.T) {
	for _, id := range []string{"workbuddy", "hermes"} {
		for _, action := range []string{"Close", "Disconnect"} {
			t.Run(id+"/"+action, func(t *testing.T) {
				f := entryLifecycleSetup(t, id)
				// A nonempty directory cannot be replaced by the real adapter.
				saved := f.a.Path + ".held-by-test"
				if err := os.Rename(f.a.Path, saved); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(f.a.Path, 0700); err != nil {
					t.Fatal(err)
				}
				blocker := filepath.Join(f.a.Path, "occupied")
				if err := os.WriteFile(blocker, []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
				restore := f.a.Disconnect
				if action == "Close" {
					restore = f.m.Close
				}
				if err := restore(); err == nil {
					t.Fatal("occupied target accepted restoration")
				}
				if v, ok := managedOwners.Load(appdir.Config()); !ok || v != f.m {
					t.Fatal("failed restore lost controller")
				}
				if f.m.slots[id].session == nil {
					t.Fatal("failed restore lost retry session")
				}
				if _, err := EnableManagedEntries(f.store); !errors.Is(err, ErrManagedLocked) {
					t.Errorf("replacement during failed recovery: %v", err)
				}
				if action == "Close" {
					before := entryLifecycleSnapshot(t, f)
					if err := f.a.Apply(f.key, f.value); !errors.Is(err, ErrManagedOwnerRequired) {
						t.Errorf("write after failed Close = %v", err)
					}
					if !reflect.DeepEqual(before, entryLifecycleSnapshot(t, f)) {
						t.Error("rejected write after failed Close modified recovery files")
					}
				}
				if err := os.Remove(blocker); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(f.a.Path); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(saved, f.a.Path); err != nil {
					t.Fatal(err)
				}
				if err := restore(); err != nil {
					t.Fatal(err)
				}
				entryLifecycleBaseline(t, f)
				if action == "Disconnect" {
					if err := f.m.Close(); err != nil {
						t.Fatal(err)
					}
				}
				n := entryLifecycleEnable(t, f.store)
				if err := f.a.Apply(f.key, f.value); err != nil {
					t.Fatal(err)
				}
				if err := n.Close(); err != nil {
					t.Fatal(err)
				}
				entryLifecycleBaseline(t, f)
			})
		}
	}
}

func TestManagedEntryLifecycleDisconnectAndRestoreDrainActiveWrite(t *testing.T) {
	for _, id := range []string{"workbuddy", "hermes"} {
		t.Run(id, func(t *testing.T) {
			f := entryLifecycleSetup(t, id)
			entered, release := make(chan struct{}), make(chan struct{})
			written, restored, disconnected := make(chan error, 1), make(chan error, 1), make(chan error, 1)
			loaded := make(chan struct{})
			f.m.disconnectLoaded = func() { close(loaded) }
			go func() {
				_, err := managedEntryWrite(f.a, false, func(scoped *Agent) error {
					close(entered)
					<-release
					return scoped.apply(f.key, f.value)
				})
				written <- err
			}()
			<-entered
			go func() { restored <- f.m.Restore() }()
			// Wait for Restore's admission closure while the slot is still busy.
			for {
				f.m.mu.Lock()
				blocked := !f.m.recovered
				f.m.mu.Unlock()
				if blocked {
					break
				}
				runtime.Gosched()
			}
			go func() { disconnected <- f.a.Disconnect() }()
			<-loaded
			close(release)
			for _, result := range []chan error{written, restored, disconnected} {
				if err := <-result; err != nil {
					t.Error(err)
				}
			}
			entryLifecycleBaseline(t, f)
			before := entryLifecycleSnapshot(t, f)
			if err := f.a.Apply(f.key, f.value); !errors.Is(err, ErrManagedOwnerRequired) {
				t.Errorf("ownerless write = %v", err)
			}
			if !reflect.DeepEqual(before, entryLifecycleSnapshot(t, f)) {
				t.Error("ownerless write changed files")
			}
		})
	}
}
