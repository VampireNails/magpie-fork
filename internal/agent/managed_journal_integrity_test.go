package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/steady"
)

// Mutate literal object members without decoding into a map: duplicate keys
// (including escaped names) must reach the actual store unchanged.
func managedCorruptField(t *testing.T, data []byte, key, mutation string) []byte {
	t.Helper()
	needle := []byte(`"` + key + `":`)
	start := bytes.Index(data, needle)
	if start < 0 {
		t.Fatal("fixture member missing")
	}
	valueStart := start + len(needle)
	d := json.NewDecoder(bytes.NewReader(data[valueStart:]))
	var raw json.RawMessage
	if err := d.Decode(&raw); err != nil {
		t.Fatal("fixture member invalid")
	}
	end := valueStart + int(d.InputOffset())
	replacement := []byte(mutation)
	if mutation == "missing" {
		if end < len(data) && data[end] == ',' {
			end++
		} else {
			start-- // remove the preceding comma for a final member
		}
		replacement = nil
	} else if mutation == "duplicate" {
		replacement = append(append(append([]byte{}, data[start:end]...), ','), data[start:end]...)
	} else if mutation == "escaped-duplicate" {
		encoded := fmt.Sprintf(`"\u%04x%s":`, key[0], key[1:])
		replacement = []byte(string(data[start:end]) + "," + encoded + string(raw))
	} else if mutation == "case-alias" {
		replacement = []byte(string(data[start:end]) + `,"` + strings.ToUpper(key) + `":` + string(raw))
	} else {
		start = valueStart
	}
	return append(append(append([]byte{}, data[:start]...), replacement...), data[end:]...)
}

func managedAssertUnknownUntouched(t *testing.T, s *ManagedStore, a ManagedAdapter, target ManagedTarget, data []byte) {
	t.Helper()
	managedPut(t, s.path("alpha"), string(data))
	before := managedRead(t, target.Path)
	calls := 0
	guard := func() { calls++ }
	a.Validate = func(ManagedTarget) error { guard(); return nil }
	a.Capture = func(ManagedTarget) (ManagedSnapshot, error) { guard(); return ManagedSnapshot{}, nil }
	a.Cleanup = func() error { guard(); return nil }
	a.Restore = func(ManagedTarget, ManagedSnapshot) error { guard(); return nil }
	a.Finalize = func(ManagedFileBaseline) error { guard(); return nil }
	state, stateErr := s.ManagedState("alpha")
	if !errors.Is(stateErr, ErrManagedUnknown) || state.Phase != ManagedUnknown || state.Generation != 0 || state.Pending != 0 || state.LastErrorCode != "" {
		t.Error("damaged journal did not produce unknown status")
	}
	restoreErr := s.RestoreManaged("alpha", a)
	if !errors.Is(restoreErr, ErrManagedUnknown) {
		t.Error("damaged journal authorized restore")
	}
	_, beginErr := s.BeginManaged("alpha", a, target)
	if !errors.Is(beginErr, ErrManagedUnknown) {
		t.Error("damaged journal authorized begin")
	}
	if calls != 0 || managedRead(t, target.Path) != before || managedRead(t, s.path("alpha")) != string(data) {
		t.Error("damaged journal ran a hook, changed config, or overwrote original bytes")
	}
	if strings.Contains(fmt.Sprint(state, stateErr, restoreErr, beginErr), "CANARY") {
		t.Error("damaged journal leaked private data")
	}
}

func TestManagedJournalIntegrityRejectsAmbiguousFields(t *testing.T) {
	fields := []struct{ key, wrong string }{
		{"schema", `"1"`}, {"generation", `false`}, {"agentId", `0`}, {"phase", `[]`},
		{"targets", `{}`}, {"files", `false`}, {"cleanupDone", `0`},
		{"target", `[]`}, {"path", `false`}, {"scope", `0`}, {"kind", `{}`},
		{"snapshot", `[]`}, {"exists", `0`}, {"fileExists", `"true"`}, {"mode", `false`},
		{"value", `[]`}, {"restored", `0`}, {"baseline", `[]`}, {"finalized", `0`},
	}
	for _, phase := range []string{"prepared", "active", "restoring"} {
		for _, field := range fields {
			for _, mutation := range []string{"null", field.wrong, "missing", "duplicate", "escaped-duplicate", "case-alias"} {
				if field.key == "value" && mutation == "null" {
					continue // nil snapshot bytes are explicitly legal
				}
				t.Run(phase+"/"+field.key+"/"+mutation, func(t *testing.T) {
					s, a, target := managedFixture(t)
					managedPut(t, target.Path, "CANARY-private-baseline")
					session := managedBegin(t, s, a, target)
					if phase != "prepared" {
						managedChange(t, session, target, "CANARY-managed")
					}
					if phase == "restoring" {
						a.Cleanup = func() error { return errors.New("CANARY-cleanup") }
						a.Restore = func(ManagedTarget, ManagedSnapshot) error { return errors.New("CANARY-restore") }
						if err := s.RestoreManaged("alpha", a); !errors.Is(err, ErrManagedRestore) {
							t.Fatal("fixture restore failure not retained")
						}
					}
					data := []byte(managedRead(t, s.path("alpha")))
					managedAssertUnknownUntouched(t, s, a, target, managedCorruptField(t, data, field.key, mutation))
				})
			}
		}
	}
}

func TestManagedJournalIntegrityEmptyArraysAreNotNull(t *testing.T) {
	for _, phase := range []string{"prepared", "active", "restoring", "detached"} {
		for _, field := range []string{"targets", "files", "cleanupDone", "schema", "generation", "agentId", "phase"} {
			t.Run(phase+"/"+field, func(t *testing.T) {
				s, a, target := managedFixture(t)
				managedPut(t, target.Path, "CANARY-unowned")
				session := managedBegin(t, s, a)
				if phase == "active" {
					if err := session.BeforeManagedWrite(func(*ManagedWrite) error { return nil }); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "restoring" || phase == "detached" {
					a.Cleanup = func() error { return errors.New("CANARY-cleanup") }
					if phase == "detached" {
						a.Cleanup = nil
					}
					err := s.RestoreManaged("alpha", a)
					if phase == "restoring" && !errors.Is(err, ErrManagedRestore) || phase == "detached" && err != nil {
						t.Fatal("fixture phase not retained")
					}
				}
				data := []byte(managedRead(t, s.path("alpha")))
				// Old writer emits null; establish an explicit empty array fixture
				// before independently corrupting each required member.
				data = bytes.ReplaceAll(data, []byte(`"targets":null`), []byte(`"targets":[]`))
				data = bytes.ReplaceAll(data, []byte(`"files":null`), []byte(`"files":[]`))
				managedAssertUnknownUntouched(t, s, a, target, managedCorruptField(t, data, field, "null"))
			})
		}
	}
}

func TestManagedJournalIntegrityZeroTargetsRoundTripAndAppend(t *testing.T) {
	s, a, target := managedFixture(t)
	managedPut(t, target.Path, "baseline")
	session := managedBegin(t, s, a)
	check := func(phase ManagedPhase, empty bool) {
		t.Helper()
		reopened, _ := NewManagedStore(s.dir)
		state, err := reopened.ManagedState("alpha")
		if err != nil || state.Phase != phase {
			t.Fatal("writer cannot read its own journal phase")
		}
		if empty {
			data := managedRead(t, s.path("alpha"))
			if !strings.Contains(data, `"targets":[]`) || !strings.Contains(data, `"files":[]`) {
				t.Error("empty recovery arrays were not serialized as arrays")
			}
		}
	}
	check(ManagedPrepared, true)
	if err := session.BeforeManagedWrite(func(*ManagedWrite) error { return nil }); err != nil {
		t.Fatal(err)
	}
	check(ManagedActive, true)
	managedChange(t, session, target, "managed")
	check(ManagedActive, false)
	if err := s.RestoreManaged("alpha", a); err != nil || managedRead(t, target.Path) != "baseline" {
		t.Fatal("dynamic target did not restore")
	}
	check(ManagedDetached, true)
	managedBegin(t, s, a) // a detached journal must also permit the next generation
}

func TestManagedJournalIntegrityCheckpointFailureRetriesHooks(t *testing.T) {
	for _, hook := range []string{"cleanup", "restore", "finalize"} {
		t.Run(hook, func(t *testing.T) {
			s, a, target := managedFixture(t)
			managedPut(t, target.Path, "baseline")
			session := managedBegin(t, s, a, target)
			managedChange(t, session, target, "managed")
			oldSync := steady.Sync
			t.Cleanup(func() { steady.Sync = oldSync })
			calls := map[string]int{}
			fail := true
			afterHook := func(name string) {
				calls[name]++
				if fail && hook == name {
					steady.Sync = func(*os.File) error { return errors.New("CANARY-checkpoint") }
				}
			}
			a.Cleanup = func() error { afterHook("cleanup"); return nil }
			restore := a.Restore
			a.Restore = func(v ManagedTarget, snap ManagedSnapshot) error {
				if err := restore(v, snap); err != nil {
					return err
				}
				afterHook("restore")
				return nil
			}
			a.Finalize = func(ManagedFileBaseline) error { afterHook("finalize"); return nil }
			if err := s.RestoreManaged("alpha", a); !errors.Is(err, ErrManagedStore) || strings.Contains(fmt.Sprint(err), "CANARY") {
				t.Fatal("failed checkpoint not reported safely")
			}
			steady.Sync = oldSync
			checkpoint := managedRead(t, s.path("alpha"))
			state, err := s.ManagedState("alpha")
			if err != nil || state.Phase != ManagedRestoring || managedRead(t, s.path("alpha")) != checkpoint {
				t.Fatal("partial checkpoint cannot be reopened without rewriting")
			}
			fail = false
			if err := s.RestoreManaged("alpha", a); err != nil || managedRead(t, target.Path) != "baseline" {
				t.Fatal("checkpoint retry did not restore")
			}
			for _, name := range []string{"cleanup", "restore", "finalize"} {
				want := 1
				if name == hook {
					want = 2
				}
				if calls[name] != want {
					t.Errorf("%s calls=%d want=%d", name, calls[name], want)
				}
			}
			if err := s.RestoreManaged("alpha", a); err != nil {
				t.Fatal(err)
			}
		})
	}
}
