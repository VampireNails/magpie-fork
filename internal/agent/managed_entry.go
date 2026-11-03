package agent

import (
	"encoding/json"
	"errors"
	"github.com/yetone/magpie/internal/edit"
	"os"
	"path/filepath"
	"sync"

	"github.com/yetone/magpie/internal/appdir"
)

// ErrManagedOwnerRequired prevents a separate CLI process from silently
// bypassing the resident remote client's recovery transaction.
var ErrManagedOwnerRequired = errors.New("remote client: start the resident owner before connecting this agent")

// managedEntryBlocked is conservative until the resident transaction owner
// is installed. An unreadable marker is not evidence of an unconfigured client.
func managedEntryBlocked(a *Agent) bool {
	if a.ID != "workbuddy" && a.ID != "hermes" {
		return false
	}
	for _, p := range []string{
		filepath.Join(appdir.Config(), "remote-connection.json"),
		filepath.Join(appdir.Config(), "remote-agent-sessions", a.ID+".json"),
	} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
}

// ManagedEntries is installed only by a runner that holds the OS owner lock.
type ManagedEntries struct {
	mu                          sync.Mutex
	lifecycle                   sync.Mutex
	key                         string
	closed                      bool
	store                       *ManagedStore
	recovered, owner, connected bool
	slots                       map[string]*managedEntrySlot
	// disconnectLoaded is an immutable test barrier; production leaves it nil.
	disconnectLoaded func()
}
type managedEntrySlot struct {
	mu      sync.Mutex
	session *ManagedSession
}

var managedOwners sync.Map // config directory -> *ManagedEntries

func EnableManagedEntries(store *ManagedStore) (*ManagedEntries, error) {
	if store == nil {
		return nil, ErrManagedInvalid
	}
	m := &ManagedEntries{store: store, key: appdir.Config(), slots: map[string]*managedEntrySlot{"workbuddy": {}, "hermes": {}}}
	if _, loaded := managedOwners.LoadOrStore(m.key, m); loaded {
		return nil, ErrManagedLocked
	}
	return m, nil
}
func (m *ManagedEntries) SetConnection(owner, connected bool) {
	m.mu.Lock()
	if !m.closed && m.recovered {
		m.owner = owner
		m.connected = connected
	}
	m.mu.Unlock()
}
func (m *ManagedEntries) Recover() error { return m.Restore() }
func (m *ManagedEntries) Restore() error {
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	return m.restore()
}
func (m *ManagedEntries) Close() error {
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	if err := m.restore(); err != nil {
		return err
	}
	m.mu.Lock()
	m.closed = true
	m.recovered = false
	m.mu.Unlock()
	managedOwners.CompareAndDelete(m.key, m)
	return nil
}
func (m *ManagedEntries) restore() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrManagedStale
	}
	m.owner = false
	m.connected = false
	m.recovered = false
	m.mu.Unlock()
	home, _ := homes()
	var errs []error
	for _, a := range []*Agent{workbuddy(home), hermes(home)} {
		slot := m.slots[a.ID]
		slot.mu.Lock()
		adapter, _, err := managedAdapterFor(a)
		if err == nil {
			err = m.store.RestoreManaged(a.ID, adapter)
		}
		if err == nil {
			slot.session = nil
		} else {
			errs = append(errs, err)
		}
		slot.mu.Unlock()
	}
	err := errors.Join(errs...)
	if err == nil {
		m.mu.Lock()
		m.recovered = true
		m.mu.Unlock()
	}
	return err
}
func managedAdapterFor(a *Agent) (ManagedAdapter, []ManagedTarget, error) {
	if a.ID == "workbuddy" {
		ad, targets, err := ManagedWorkBuddy(a)
		ad.Cleanup = managedWorkBuddyCleanup
		return ad, targets, err
	}
	home, _ := homes()
	return HermesManagedAdapter(hermesRoot(here(home))), nil, nil
}

// managedWorkBuddyCleanup removes only product bookkeeping for WorkBuddy.
func managedWorkBuddyCleanup() error {
	stashMu.Lock()
	err := func() error {
		raw, e := os.ReadFile(stashPath())
		if errors.Is(e, os.ErrNotExist) {
			return nil
		}
		if e != nil {
			return e
		}
		var values map[string]string
		if json.Unmarshal(raw, &values) != nil || values == nil {
			return ErrManagedRestore
		}
		delete(values, "workbuddy.connect.was")
		delete(values, "workbuddy.reconnect")
		values["workbuddy"+disconnectedKey] = "1"
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
	delete(values, "workbuddy")
	raw, err = json.MarshalIndent(values, "", "  ")
	if err != nil {
		return err
	}
	return edit.WriteAtomic(appliedPath(), raw)
}
func managedEntryWrite(a *Agent, explicit bool, fn func(*Agent) error) (bool, error) {
	if a.ID != "workbuddy" && a.ID != "hermes" {
		return false, nil
	}
	v, ok := managedOwners.Load(appdir.Config())
	if !ok {
		if managedEntryBlocked(a) {
			return true, ErrManagedOwnerRequired
		}
		return false, nil
	}
	m := v.(*ManagedEntries)
	slot := m.slots[a.ID]
	slot.mu.Lock()
	defer slot.mu.Unlock()
	m.mu.Lock()
	ready := m.recovered && m.owner && m.connected
	m.mu.Unlock()
	if !ready {
		return true, ErrManagedOwnerRequired
	}
	if slot.session == nil {
		if !explicit {
			return true, nil
		}
		adapter, targets, err := managedAdapterFor(a)
		if err != nil {
			return true, err
		}
		slot.session, err = m.store.BeginManaged(a.ID, adapter, targets...)
		if err != nil {
			return true, err
		}
	}
	err := slot.session.BeforeManagedWrite(func(w *ManagedWrite) error {
		if a.ID == "workbuddy" {
			if err := BeforeManagedWorkBuddyWrite(w, a); err != nil {
				return err
			}
			return fn(a)
		}
		home, _ := homes()
		return WithManagedHermes(w, home, fn)
	})
	return true, err
}

func managedEntryDisconnect(a *Agent) (bool, error) {
	if a.ID != "workbuddy" && a.ID != "hermes" {
		return false, nil
	}
	v, ok := managedOwners.Load(appdir.Config())
	if !ok {
		if managedEntryBlocked(a) {
			return true, ErrManagedOwnerRequired
		}
		return false, nil
	}
	m := v.(*ManagedEntries)
	if m.disconnectLoaded != nil {
		m.disconnectLoaded()
	}
	// Keep Close from unregistering this controller between validation and
	// restore. The journal is keyed by agent, so it could belong to a new owner.
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	slot := m.slots[a.ID]
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if current, ok := managedOwners.Load(m.key); m.closed || !ok || current != m {
		return true, ErrManagedStale
	}
	adapter, _, err := managedAdapterFor(a)
	if err == nil {
		err = m.store.RestoreManaged(a.ID, adapter)
	}
	if err == nil {
		slot.session = nil
	}
	return true, err
}
