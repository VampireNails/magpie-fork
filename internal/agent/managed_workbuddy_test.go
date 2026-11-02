package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
)

func TestManagedWorkBuddyCaptureFailureStopsNativeConnector(t *testing.T) {
	a := managedWorkBuddyFixture(t)
	const original = `{"models":[],"custom":"synthetic-original"}`
	managedWorkBuddyPut(t, a.Path, original, 0o600)
	adapter, _, err := ManagedWorkBuddy(a)
	if err != nil {
		t.Fatal(err)
	}
	const canary = "CANARY-workbuddy-private-574"
	adapter.Capture = func(ManagedTarget) (ManagedSnapshot, error) { return ManagedSnapshot{}, errors.New(canary) }
	store, err := NewManagedStore(filepath.Join(t.TempDir(), "journal"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.BeginManaged(a.ID, adapter)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	err = session.BeforeManagedWrite(func(w *ManagedWrite) error {
		if err := BeforeManagedWorkBuddyWrite(w, a); err != nil {
			return err
		}
		called = true
		return a.Field("provider").Set(magpieID)
	})
	if !errors.Is(err, ErrManagedCapture) || called {
		t.Fatal("capture failure did not stop native Set")
	}
	status, statusErr := store.ManagedState(a.ID)
	encoded, _ := json.Marshal(status)
	if statusErr != nil || strings.Contains(fmt.Sprint(err), canary) || strings.Contains(string(encoded), canary) {
		t.Fatal("capture diagnostics unavailable or leaked private data")
	}
	if !bytes.Equal(managedWorkBuddyBytes(t, a.Path), []byte(original)) {
		t.Fatal("capture failure changed models file")
	}
	if err := store.RestoreManaged(a.ID, adapter); err != nil {
		t.Fatal(err)
	}
}

func TestManagedWorkBuddyUnknownLaterFileStopsWrite(t *testing.T) {
	a := managedWorkBuddyFixture(t)
	managedWorkBuddyPut(t, a.Path, "[]\n", 0o600)
	store, session, adapter := managedWorkBuddyBegin(t, a)
	managedWorkBuddyWrite(t, a, session, func() error { return a.Field("provider").Set(magpieID) })
	managedWorkBuddyPut(t, a.Path, "null", 0o600)
	called := false
	err := session.BeforeManagedWrite(func(w *ManagedWrite) error {
		if err := BeforeManagedWorkBuddyWrite(w, a); err != nil {
			return err
		}
		called = true
		return a.Field("provider").Set(magpieID)
	})
	if err == nil || called || string(managedWorkBuddyBytes(t, a.Path)) != "null" {
		t.Fatal("unknown current file was overwritten")
	}
	if err := store.RestoreManaged(a.ID, adapter); err != nil {
		t.Fatal(err)
	}
	if string(managedWorkBuddyBytes(t, a.Path)) != "[]\n" {
		t.Fatal("baseline lost after rejected write")
	}
}

func TestManagedWorkBuddyPersistenceFailurePreventsJoin(t *testing.T) {
	a := managedWorkBuddyFixture(t)
	adapter, targets, err := ManagedWorkBuddy(a)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(dir, []byte("obstruction"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewManagedStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginManaged(a.ID, adapter, targets...); !errors.Is(err, ErrManagedStore) {
		t.Fatal("journal failure authorized join")
	}
	if _, err := os.Stat(a.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed capture created agent directory")
	}
}

func TestManagedWorkBuddyRestoreRetryAndIsolation(t *testing.T) {
	a := managedWorkBuddyFixture(t)
	const original = "[ {\"id\":\"custom\",\"apiKey\":\"CANARY-model-private-214\"} ]\r\n"
	managedWorkBuddyPut(t, a.Path, original, 0o600)
	untouched := []string{filepath.Join(a.Dir, "settings.json"), filepath.Join(a.Dir, "keyblob"), filepath.Join(a.Dir, "sessions", "fixture"), filepath.Join(filepath.Dir(a.Dir), ".hermes", "config.yaml")}
	for _, path := range untouched {
		managedWorkBuddyPut(t, path, "unrelated-fixture", 0o600)
	}
	store, session, adapter := managedWorkBuddyBegin(t, a)
	managedWorkBuddyWrite(t, a, session, func() error { return a.Field("provider").Set(magpieID) })
	restore := adapter.Restore
	adapter.Restore = func(ManagedTarget, ManagedSnapshot) error { return errors.New("CANARY-restore-private-119") }
	if err := store.RestoreManaged(a.ID, adapter); !errors.Is(err, ErrManagedRestore) || strings.Contains(fmt.Sprint(err), "CANARY") {
		t.Fatal("restore failure missing or private data exposed")
	}
	status, err := store.ManagedState(a.ID)
	if err != nil || status.Phase != ManagedRestoring || status.Pending != 1 {
		t.Fatal("restore failure lost pending snapshot")
	}
	encoded, _ := json.Marshal(status)
	if strings.Contains(string(encoded), "CANARY") || strings.Contains(string(encoded), "apiKey") {
		t.Fatal("status exposed snapshot")
	}
	adapter.Restore = restore
	// A new process/store reads the pending baseline from disk.
	reopened, err := NewManagedStore(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.RestoreManaged(a.ID, adapter); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(managedWorkBuddyBytes(t, a.Path), []byte(original)) {
		t.Fatal("retry did not restore B0")
	}
	if err := reopened.RestoreManaged(a.ID, adapter); err != nil {
		t.Fatal("repeated restore:", err)
	}
	for _, path := range untouched {
		if string(managedWorkBuddyBytes(t, path)) != "unrelated-fixture" {
			t.Fatal("unrelated configuration changed")
		}
	}
}

func TestManagedWorkBuddyOwnershipAndPaths(t *testing.T) {
	a := managedWorkBuddyFixture(t)
	adapter, targets, err := ManagedWorkBuddy(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*ManagedTarget){
		func(t *ManagedTarget) { t.Scope = "other" }, func(t *ManagedTarget) { t.Kind = "other" },
		func(t *ManagedTarget) { t.Path = filepath.Join(a.Dir, "settings.json") },
	} {
		target := targets[0]
		change(&target)
		if adapter.Validate(target) == nil {
			t.Fatal("foreign target accepted")
		}
	}
	other := *a
	other.ID = "workbuddy-ai"
	if _, _, err := ManagedWorkBuddy(&other); err == nil {
		t.Fatal("other agent accepted")
	}
	if _, _, err := ManagedWorkBuddy(nil); err == nil {
		t.Fatal("nil connector accepted")
	}
	managedWorkBuddyPut(t, a.Dir, "parent-is-a-file", 0o600)
	if _, _, err := ManagedWorkBuddy(a); err == nil {
		t.Fatal("file used as directory accepted")
	}
}

func TestManagedWorkBuddyFileOccupiedRetry(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Log("Windows sharing violation exercised on Windows; Linux permission failure below")
		return
	}
	a := managedWorkBuddyFixture(t)
	const original = "[]\r\n"
	managedWorkBuddyPut(t, a.Path, original, 0o600)
	store, session, adapter := managedWorkBuddyBegin(t, a)
	managedWorkBuddyWrite(t, a, session, func() error { return a.Field("provider").Set(magpieID) })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := proc.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `$f=[IO.File]::Open($env:MAGPIE_TEST_HELD_FILE,[IO.FileMode]::Open,[IO.FileAccess]::ReadWrite,[IO.FileShare]::None); [Console]::Out.WriteLine('held'); [Console]::Out.Flush(); [Console]::ReadLine() | Out-Null; $f.Dispose()`)
	cmd.Env = append(os.Environ(), "MAGPIE_TEST_HELD_FILE="+a.Path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			input.Close()
			if err := cmd.Wait(); err != nil {
				t.Error("file holder failed:", err)
			}
		}
	}
	t.Cleanup(release)
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || strings.TrimSpace(line) != "held" {
		t.Fatalf("file holder did not acquire handle: %s", stderr.String())
	}
	if err := store.RestoreManaged(a.ID, adapter); !errors.Is(err, ErrManagedRestore) {
		t.Fatal("occupied file reported restored")
	}
	release()
	if err := store.RestoreManaged(a.ID, adapter); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(managedWorkBuddyBytes(t, a.Path), []byte(original)) {
		t.Fatal("retry after sharing violation lost B0")
	}
}

func TestManagedWorkBuddyLinuxFileFailures(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Log("Linux permission and symlink checks run on Linux")
		return
	}
	a := managedWorkBuddyFixture(t)
	const original = "[]\n"
	managedWorkBuddyPut(t, a.Path, original, 0o600)
	adapter, targets, err := ManagedWorkBuddy(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(a.Path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(a.Path, 0o600); _ = os.Chmod(a.Dir, 0o755) })
	if _, err := adapter.Capture(targets[0]); err == nil {
		t.Fatal("unreadable models file captured as empty")
	}
	if err := os.Chmod(a.Path, 0o600); err != nil {
		t.Fatal(err)
	}
	store, session, adapter := managedWorkBuddyBegin(t, a)
	managedWorkBuddyWrite(t, a, session, func() error { return a.Field("provider").Set(magpieID) })
	if err := os.Chmod(a.Dir, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := store.RestoreManaged(a.ID, adapter); !errors.Is(err, ErrManagedRestore) {
		t.Fatal("unwritable directory reported restored")
	}
	if err := os.Chmod(a.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.RestoreManaged(a.ID, adapter); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(managedWorkBuddyBytes(t, a.Path), []byte(original)) {
		t.Fatal("permission retry lost B0")
	}
	if err := os.Remove(a.Path); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other.json")
	managedWorkBuddyPut(t, other, original, 0o600)
	if err := os.Symlink(other, a.Path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ManagedWorkBuddy(a); err == nil {
		t.Fatal("symbolic link accepted")
	}
	if !bytes.Equal(managedWorkBuddyBytes(t, other), []byte(original)) {
		t.Fatal("symlink target changed")
	}
}

func managedWorkBuddyBegin(t *testing.T, a *Agent) (*ManagedStore, *ManagedSession, ManagedAdapter) {
	t.Helper()
	adapter, targets, err := ManagedWorkBuddy(a)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewManagedStore(filepath.Join(t.TempDir(), "journals"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.BeginManaged(a.ID, adapter, targets...)
	if err != nil {
		t.Fatal(err)
	}
	return store, session, adapter
}

func managedWorkBuddyWrite(t *testing.T, a *Agent, s *ManagedSession, fn func() error) {
	t.Helper()
	if err := s.BeforeManagedWrite(func(w *ManagedWrite) error {
		if err := BeforeManagedWorkBuddyWrite(w, a); err != nil {
			return err
		}
		return fn()
	}); err != nil {
		t.Fatal(err)
	}
}

func TestManagedWorkBuddyRestoresOriginalAbsence(t *testing.T) {
	a := managedWorkBuddyFixture(t)
	store, session, adapter := managedWorkBuddyBegin(t, a)
	managedWorkBuddyWrite(t, a, session, func() error { return a.Field("provider").Set(magpieID) })
	if _, err := os.Stat(a.Path); err != nil {
		t.Fatal("native connector did not write file:", err)
	}
	if err := store.RestoreManaged(a.ID, adapter); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("originally absent models.json remains after restore")
	}
}

func TestManagedWorkBuddyRestoresExactBytesAfterManualEdit(t *testing.T) {
	for name, original := range map[string]string{
		"array":        "[ {\"id\":\"own\", \"vendor\":\"custom\",\"apiKey\":\"synthetic-own\"} ]\r\n",
		"object":       "{\r\n \"models\": [{\"id\":\"old\",\"vendor\":\"magpie\",\"apiKey\":\"synthetic-old\"},{\"id\":\"renamed\",\"vendor\":\"Custom\",\"apiKey\":\"synthetic-old\"}], \"availableModels\": [\"old\",\"renamed\",\"own\"],\"other\":{} }\r\n",
		"key-renamed":  `[{"id":"renamed","vendor":"Custom","apiKey":"magpie-workbuddy"}]`,
		"empty-object": "{}\r\n", "empty-array": "[]\n",
		"available-empty": `{"models":[],"availableModels":[],"custom":true}`,
		"bom":             "\xef\xbb\xbf{\"models\":[]}",
	} {
		t.Run(name, func(t *testing.T) {
			a := managedWorkBuddyFixture(t)
			managedWorkBuddyPut(t, a.Path, original, 0o640)
			baselineMode, err := os.Stat(a.Path)
			if err != nil {
				t.Fatal(err)
			}
			store, session, adapter := managedWorkBuddyBegin(t, a)
			managedWorkBuddyWrite(t, a, session, func() error { return a.Field("provider").Set(magpieID) })
			for range 3 {
				managedWorkBuddyWrite(t, a, session, a.Sync)
			}
			managedWorkBuddyPut(t, a.Path, `{"models":[],"manual":"after-join"}`, 0o600)
			if err := store.RestoreManaged(a.ID, adapter); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(managedWorkBuddyBytes(t, a.Path), []byte(original)) {
				t.Fatal("restoration did not return exact B0 bytes")
			}
			fi, err := os.Stat(a.Path)
			if err != nil || fi.Mode().Perm() != baselineMode.Mode().Perm() {
				t.Fatal("original file mode not restored")
			}
		})
	}
}

func TestManagedWorkBuddyRejectsUnknownDocument(t *testing.T) {
	for name, data := range map[string]string{
		"empty": "", "null": "null", "scalar": `"synthetic-secret"`,
		"models-null": `{"models":null}`, "models-object": `{"models":{}}`,
		"available-object":  `{"models":[],"availableModels":{}}`,
		"available-numbers": `{"availableModels":[1]}`, "entry-scalar": `["synthetic-secret"]`,
	} {
		t.Run(name, func(t *testing.T) {
			a := managedWorkBuddyFixture(t)
			managedWorkBuddyPut(t, a.Path, data, 0o600)
			if err := validateManagedWorkBuddy(a.Path); err == nil {
				t.Fatal("unknown document accepted as empty configuration")
			}
		})
	}
}

// These fixtures exercise the native connector, with synthetic credentials.
// No fixture or failure message contains a user's configuration.
func managedWorkBuddyFixture(t *testing.T) *Agent {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("APPDATA", filepath.Join(home, "appdata"))
	t.Setenv("WORKBUDDY_CONFIG_DIR", filepath.Join(home, "wb"))
	if err := provider.Save(provider.Provider{ID: "fixture", Name: "Fixture", Chat: "https://fixture.invalid/v1", Key: "synthetic-provider", Models: []string{"one", "two"}}); err != nil {
		t.Fatal("save fixture provider:", err)
	}
	return workbuddy(home)
}

func managedWorkBuddyPut(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func managedWorkBuddyBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read fixture:", err)
	}
	return b
}

func TestManagedWorkBuddyRestoreOppositeBOM(t *testing.T) {
	for _, baselineBOM := range []bool{false, true} {
		t.Run(fmt.Sprint(baselineBOM), func(t *testing.T) {
			a := managedWorkBuddyFixture(t)
			original, current := "[]\r\n", "{\"models\":[],\"manual\":true}\n"
			if baselineBOM {
				original = "\xef\xbb\xbf" + original
			} else {
				current = "\xef\xbb\xbf" + current
			}
			managedWorkBuddyPut(t, a.Path, original, 0o640)
			before, err := os.Stat(a.Path)
			if err != nil {
				t.Fatal(err)
			}
			store, session, adapter := managedWorkBuddyBegin(t, a)
			managedWorkBuddyWrite(t, a, session, func() error { return a.Field("provider").Set(magpieID) })
			managedWorkBuddyPut(t, a.Path, current, 0o600)
			if err := store.RestoreManaged(a.ID, adapter); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(managedWorkBuddyBytes(t, a.Path), []byte(original)) {
				t.Fatal("restore inherited current BOM instead of exact B0")
			}
			after, err := os.Stat(a.Path)
			if err != nil || after.Mode().Perm() != before.Mode().Perm() {
				t.Fatal("B0 mode not restored")
			}
		})
	}
}

func TestManagedWorkBuddyRestoreBypassesEditFilter(t *testing.T) {
	a := managedWorkBuddyFixture(t)
	const original = "[]\n"
	managedWorkBuddyPut(t, a.Path, original, 0o600)
	store, session, adapter := managedWorkBuddyBegin(t, a)
	managedWorkBuddyWrite(t, a, session, func() error { return a.Field("provider").Set(magpieID) })
	if err := edit.Filtered(func(path string, data []byte) []byte {
		if path == a.Path {
			return []byte("{\"models\":[],\"filter\":true}")
		}
		return data
	}, func() error { return store.RestoreManaged(a.ID, adapter) }); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(managedWorkBuddyBytes(t, a.Path), []byte(original)) {
		t.Fatal("restore transformed B0 through edit filter")
	}
}

func TestManagedWorkBuddyHardLinkBeforeJoin(t *testing.T) {
	a := managedWorkBuddyFixture(t)
	const original = "[]\n"
	managedWorkBuddyPut(t, a.Path, original, 0o600)
	other := filepath.Join(a.Dir, "other.json")
	if err := os.Link(a.Path, other); err != nil {
		t.Fatal("create same-volume hard link:", err)
	}
	adapter, targets, err := ManagedWorkBuddy(a)
	if err == nil {
		store, storeErr := NewManagedStore(filepath.Join(t.TempDir(), "journal"))
		if storeErr != nil {
			t.Fatal(storeErr)
		}
		session, beginErr := store.BeginManaged(a.ID, adapter, targets...)
		if beginErr == nil {
			err = session.BeforeManagedWrite(func(w *ManagedWrite) error {
				if err := BeforeManagedWorkBuddyWrite(w, a); err != nil {
					return err
				}
				return a.Field("provider").Set(magpieID)
			})
		} else {
			err = beginErr
		}
	}
	if !bytes.Equal(managedWorkBuddyBytes(t, other), []byte(original)) {
		t.Error("native join changed non-target hard-link inode")
	}
	if err == nil {
		t.Error("hard-linked target authorized join")
	}
}

func TestManagedWorkBuddyHardLinkReplacement(t *testing.T) {
	for _, operation := range []string{"restore", "set", "sync"} {
		t.Run(operation, func(t *testing.T) {
			a := managedWorkBuddyFixture(t)
			const original = "[]\n"
			const unrelated = "{\"models\":[],\"manual\":\"other-file\"}\n"
			managedWorkBuddyPut(t, a.Path, original, 0o640)
			before, err := os.Stat(a.Path)
			if err != nil {
				t.Fatal(err)
			}
			store, session, adapter := managedWorkBuddyBegin(t, a)
			managedWorkBuddyWrite(t, a, session, func() error { return a.Field("provider").Set(magpieID) })
			other := filepath.Join(a.Dir, "other.json")
			managedWorkBuddyPut(t, other, unrelated, 0o600)
			if err := os.Remove(a.Path); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(other, a.Path); err != nil {
				t.Fatal("replace with same-volume hard link:", err)
			}
			if operation == "restore" {
				err = store.RestoreManaged(a.ID, adapter)
			} else {
				err = session.BeforeManagedWrite(func(w *ManagedWrite) error {
					if err := BeforeManagedWorkBuddyWrite(w, a); err != nil {
						return err
					}
					if operation == "sync" {
						return a.Sync()
					}
					return a.Field("provider").Set(magpieID)
				})
			}
			if !bytes.Equal(managedWorkBuddyBytes(t, other), []byte(unrelated)) {
				t.Error("operation changed non-target hard-link inode")
			}
			status, statusErr := store.ManagedState(a.ID)
			if err == nil || statusErr != nil || status.Phase == ManagedDetached || status.Pending != 1 {
				t.Error("unsafe replacement did not retain retryable snapshot")
			}
			if err := os.Remove(a.Path); err != nil {
				t.Fatal(err)
			}
			managedWorkBuddyPut(t, a.Path, "{}", 0o600)
			if err := store.RestoreManaged(a.ID, adapter); err != nil {
				t.Fatal("safe-path retry:", err)
			}
			if !bytes.Equal(managedWorkBuddyBytes(t, a.Path), []byte(original)) {
				t.Error("retry lost B0 bytes")
			}
			after, err := os.Stat(a.Path)
			if err != nil || after.Mode().Perm() != before.Mode().Perm() {
				t.Error("retry lost B0 mode")
			}
			if !bytes.Equal(managedWorkBuddyBytes(t, other), []byte(unrelated)) {
				t.Error("retry modified other file")
			}
			status, err = store.ManagedState(a.ID)
			if err != nil || status.Phase != ManagedDetached || status.Pending != 0 {
				t.Error("successful retry not detached")
			}
		})
	}
}

func TestManagedWorkBuddyInvalidUTF8StopsNativeWrites(t *testing.T) {
	const canary = "CANARY-invalid-encoding-612"
	for name, data := range map[string]string{
		"model":           "[{\"id\":\"" + canary + "\xff\"}]",
		"other-field":     "{\"models\":[],\"custom\":\"" + canary + "\xff\"}",
		"availableModels": "{\"models\":[],\"availableModels\":[\"" + canary + "\xff\"]}",
		"bom":             "\xef\xbb\xbf[{\"id\":\"" + canary + "\xff\"}]",
	} {
		for _, stage := range []string{"capture", "later"} {
			for _, operation := range []string{"set", "sync"} {
				t.Run(name+"/"+stage+"/"+operation, func(t *testing.T) {
					a := managedWorkBuddyFixture(t)
					managedWorkBuddyPut(t, a.Path, "[]\n", 0o600)
					store, err := NewManagedStore(filepath.Join(t.TempDir(), "journal"))
					if err != nil {
						t.Fatal(err)
					}
					adapter, targets, err := ManagedWorkBuddy(a)
					if err != nil {
						t.Fatal(err)
					}
					var session *ManagedSession
					if stage == "later" {
						session, err = store.BeginManaged(a.ID, adapter, targets...)
						if err != nil {
							t.Fatal(err)
						}
						managedWorkBuddyWrite(t, a, session, func() error { return a.Field("provider").Set(magpieID) })
					}
					managedWorkBuddyPut(t, a.Path, data, 0o600)
					if stage == "capture" {
						session, err = store.BeginManaged(a.ID, adapter, targets...)
					}
					called := false
					if err == nil {
						err = session.BeforeManagedWrite(func(w *ManagedWrite) error {
							if err := BeforeManagedWorkBuddyWrite(w, a); err != nil {
								return err
							}
							called = true
							if operation == "sync" {
								return a.Sync()
							}
							return a.Field("provider").Set(magpieID)
						})
					}
					if err == nil || called {
						t.Error("invalid UTF-8 reached native " + operation)
					}
					if !bytes.Equal(managedWorkBuddyBytes(t, a.Path), []byte(data)) {
						t.Error("invalid UTF-8 file was changed")
					}
					status, statusErr := store.ManagedState(a.ID)
					encoded, _ := json.Marshal(status)
					if statusErr != nil || strings.Contains(fmt.Sprint(err), canary) || bytes.Contains(encoded, []byte(canary)) {
						t.Error("diagnostics unavailable or leaked secret")
					}
					if stage == "later" {
						if err := store.RestoreManaged(a.ID, adapter); err != nil {
							t.Fatal(err)
						}
						if string(managedWorkBuddyBytes(t, a.Path)) != "[]\n" {
							t.Error("rejected write lost original baseline")
						}
					}
				})
			}
		}
	}
}
