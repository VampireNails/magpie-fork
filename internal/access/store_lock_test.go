package access

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/proc"
)

func storeFixture(t *testing.T) (DeviceCredential, string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	c, err := EnsureDevice(context.Background(), DeviceIdentity{"stable-a", "1", "device"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Update("add-key", Change{Name: "admin"}); err != nil {
		t.Fatal(err)
	}
	ks, err := Export()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range ks {
		if k.Device == nil {
			return c, k.ID
		}
	}
	t.Fatal("missing admin key")
	return c, ""
}

func storeActions(c DeviceCredential, admin string) map[string]func() error {
	ctx := context.Background()
	return map[string]func() error{
		"ensure":          func() error { _, e := EnsureDevice(ctx, DeviceIdentity{"stable-b", "2", "device"}); return e },
		"remove-device":   func() error { return RemoveDevice(ctx, c.Device.ID) },
		"list-devices":    func() error { _, e := ListDevices(ctx, "", 100); return e },
		"add":             func() error { _, e := Update("add-key", Change{Name: "new"}); return e },
		"rename":          func() error { _, e := Update("rename-key", Change{Key: admin, Name: "renamed"}); return e },
		"rotate":          func() error { _, e := Update("rotate-key", Change{Key: admin}); return e },
		"remove":          func() error { _, e := Update("remove-key", Change{Key: admin}); return e },
		"restore":         func() error { return Restore(nil) },
		"migrate":         MigrateLegacyLANKey,
		"configure":       func() error { return ConfigureLAN(true, true) },
		"move-model":      func() error { return MoveModel("p/old", "p/new") },
		"replace-account": func() error { _, e := ReplaceAccount("p", "old", "new"); return e },
		"issue":           func() error { _, e := IssueKey(Issue{TTL: time.Hour}); return e },
		"revoke-issued":   func() error { return RevokeIssued("absent", "") },
		"prune":           func() error { _, e := Prune(); return e },
		"list":            func() error { _, e := List(); return e },
		"export":          func() error { _, e := Export(); return e },
		"check":           func() error { _, e := Check(c.Secret); return e },
	}
}

func TestAccessRejectsUnavailableLock(t *testing.T) {
	for name := range storeActions(DeviceCredential{}, "") {
		t.Run(name, func(t *testing.T) {
			c, admin := storeFixture(t)
			before, err := os.ReadFile(Path())
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(Path() + ".lock"); err != nil {
				t.Fatal(err)
			}
			if err = os.Mkdir(Path()+".lock", 0700); err != nil {
				t.Fatal(err)
			}
			if err = storeActions(c, admin)[name](); err == nil {
				t.Error("lock unavailable: operation reported success")
			}
			after, err := os.ReadFile(Path())
			if err != nil || !bytes.Equal(before, after) {
				t.Error("lock unavailable: caller store changed")
			}
		})
	}
}

func TestAccessRestoreRefusesCorruptStore(t *testing.T) {
	storeFixture(t)
	if err := os.WriteFile(Path(), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Restore(nil); err == nil {
		t.Error("restore overwrote an unknown store")
	}
	b, err := os.ReadFile(Path())
	if err != nil || string(b) != "broken" {
		t.Fatal("corrupt store overwritten")
	}
}

// Each worker is the same test executable with the parent's isolated HOME
// and config. Pipes establish readiness and release; no startup sleeps.
type storeChild struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	stderr bytes.Buffer
}

func startStoreChild(t *testing.T, mode, arg string) *storeChild {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	c := &storeChild{cmd: proc.CommandContext(ctx, os.Args[0], "-test.run=^TestAccessStoreProcess$", "-test.count=1")}
	c.cmd.Env = append(os.Environ(), "MAGPIE_STORE_WORKER="+mode, "MAGPIE_STORE_ARG="+arg)
	c.cmd.Stderr = &c.stderr
	var err error
	c.in, err = c.cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	out, err := c.cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	c.out = bufio.NewReader(out)
	if err = c.cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); c.in.Close(); c.cmd.Process.Kill(); c.cmd.Wait() })
	if line := childLine(t, c); line != "ready\n" {
		t.Fatal("worker did not reach barrier")
	}
	return c
}

func childLine(t *testing.T, c *storeChild) string {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() { s, e := c.out.ReadString('\n'); done <- result{s, e} }()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal("worker output ended before result")
		}
		return r.line
	case <-time.After(60 * time.Second):
		c.cmd.Process.Kill()
		<-done
		t.Fatal("worker did not respond")
		return ""
	}
}

type storeResult struct {
	ID     string
	Digest [32]byte
	Failed bool
	Reason string
}

func TestAccessStoreProcess(t *testing.T) {
	mode := os.Getenv("MAGPIE_STORE_WORKER")
	if mode == "" {
		return
	}
	arg := os.Getenv("MAGPIE_STORE_ARG")
	if os.Getenv("MAGPIE_STORE_STRESS") == "1" {
		storeLockWait = 60 * time.Second
	}
	if mode == "probe" {
		storeLockWait = 150 * time.Millisecond
	}
	if mode == "hold" {
		unlock, held := filelock.Lock(Path()+".lock", time.Second)
		if !held {
			t.Fatal("holder could not lock")
		}
		defer unlock()
		fmt.Println("ready")
		bufio.NewReader(os.Stdin).ReadString('\n')
		return
	}
	fmt.Println("ready")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	var result storeResult
	switch mode {
	case "ensure":
		c, e := EnsureDevice(context.Background(), DeviceIdentity{arg, "1", "same name"})
		result.ID, result.Digest, result.Failed = c.Device.ID, sha256.Sum256([]byte(c.Secret)), e != nil
		if e != nil {
			if errors.Is(e, context.DeadlineExceeded) {
				result.Reason = "deadline"
			} else {
				result.Reason = "other"
			}
		}
	case "admin":
		_, e := Update("add-key", Change{Name: arg})
		result.Failed = e != nil
	case "remove":
		result.Failed = RemoveDevice(context.Background(), arg) != nil
	case "probe":
		switch arg {
		case "ensure":
			_, e := EnsureDevice(context.Background(), DeviceIdentity{"new", "1", "new"})
			result.Failed = e != nil
		case "add":
			_, e := Update("add-key", Change{Name: "new"})
			result.Failed = e != nil
		case "restore":
			result.Failed = Restore(nil) != nil
		case "remove":
			result.Failed = RemoveDevice(context.Background(), os.Getenv("MAGPIE_STORE_DEVICE")) != nil
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		t.Fatal(err)
	}
}

func finishStoreChild(t *testing.T, c *storeChild) storeResult {
	t.Helper()
	var result storeResult
	if err := json.Unmarshal([]byte(childLine(t, c)), &result); err != nil {
		t.Fatal("invalid worker result")
	}
	if err := c.cmd.Wait(); err != nil {
		t.Fatal("worker failed")
	}
	return result
}

func TestAccessHeldLockNeverWrites(t *testing.T) {
	c, _ := storeFixture(t)
	t.Setenv("MAGPIE_STORE_DEVICE", c.Device.ID)
	before, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	startStoreChild(t, "hold", "")
	var children []*storeChild
	for _, action := range []string{"ensure", "add", "restore", "remove"} {
		children = append(children, startStoreChild(t, "probe", action))
	}
	for _, child := range children {
		fmt.Fprintln(child.in, "go")
	}
	for i, child := range children {
		if !finishStoreChild(t, child).Failed {
			t.Errorf("operation %d succeeded while another process held the lock", i)
		}
	}
	after, err := os.ReadFile(Path())
	if err != nil || !bytes.Equal(before, after) {
		t.Error("caller store changed while lock was held")
	}
}

func TestAccessContextCancelsWhileContended(t *testing.T) {
	storeFixture(t)
	startStoreChild(t, "hold", "")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := EnsureDevice(ctx, DeviceIdentity{"new", "1", "new"})
	if err != context.DeadlineExceeded {
		t.Error("registration did not report context deadline")
	}
	// The 3s margin is deliberately much larger than the 100ms deadline,
	// but less than the old unconditional 10s lock wait.
	if time.Since(start) > 3*time.Second {
		t.Error("context cancellation waited for the full lock timeout")
	}
}

func TestDeviceAcrossProcesses(t *testing.T) {
	t.Setenv("MAGPIE_STORE_STRESS", "1")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var first storeResult
	for batch := range 4 {
		children := make([]*storeChild, 25)
		for i := range children {
			children[i] = startStoreChild(t, "ensure", "stable-a")
		}
		for _, c := range children {
			fmt.Fprintln(c.in, "go")
		}
		for i, c := range children {
			r := finishStoreChild(t, c)
			if batch == 0 && i == 0 {
				first = r
			}
			if r.Failed || r.ID == "" || r != first {
				t.Fatalf("batch %d worker %d: failed=%v reason=%s empty=%v id_equal=%v digest_equal=%v", batch, i, r.Failed, r.Reason, r.ID == "", r.ID == first.ID, r.Digest == first.Digest)
			}
		}
	}
	ks, err := Export()
	if err != nil || len(ks) != 1 {
		t.Fatal("100 registrations did not leave exactly one key", err)
	}
}

func TestDeviceAndAdminProcessesKeepChanges(t *testing.T) {
	c, admin := storeFixture(t)
	var children []*storeChild
	for i := range 12 {
		children = append(children, startStoreChild(t, "ensure", fmt.Sprintf("stable-%d", i%2)))
		children = append(children, startStoreChild(t, "admin", fmt.Sprintf("admin-%d", i)))
	}
	for _, child := range children {
		fmt.Fprintln(child.in, "go")
	}
	for _, child := range children {
		if finishStoreChild(t, child).Failed {
			t.Fatal("concurrent operation failed")
		}
	}
	ks, err := Export()
	if err != nil || len(ks) != 16 {
		t.Fatal("concurrent registration or admin key lost", err)
	}
	found := map[string]bool{}
	for _, k := range ks {
		found[k.ID] = true
	}
	if !found[c.Device.ID] || !found[admin] {
		t.Fatal("existing key lost")
	}
}

func TestDeviceRemoveAndReregisterAcrossProcesses(t *testing.T) {
	c, admin := storeFixture(t)
	other, err := EnsureDevice(context.Background(), DeviceIdentity{"other", "2", "other"})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		remove := startStoreChild(t, "remove", c.Device.ID)
		ensure := startStoreChild(t, "ensure", "stable-a")
		fmt.Fprintln(remove.in, "go")
		fmt.Fprintln(ensure.in, "go")
		if finishStoreChild(t, remove).Failed || finishStoreChild(t, ensure).Failed {
			t.Fatal("remove/ensure failed")
		}
		c, err = EnsureDevice(context.Background(), DeviceIdentity{"stable-a", "1", "again"})
		if err != nil {
			t.Fatal(err)
		}
		ks, e := Export()
		if e != nil || len(ks) != 3 {
			t.Fatal("remove/register left duplicate or lost another key")
		}
		for _, k := range ks {
			if k.ID == other.Device.ID && k.Secret != other.Secret {
				t.Fatal("other device changed")
			}
		}
		if !On(admin) || !On(other.Device.ID) {
			t.Fatal("other key removed")
		}
	}
}
