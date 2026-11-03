//go:build !windows

package remotejoin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/proc"
)

func TestOwnerReplacementProcessHelper(t *testing.T) {
	if os.Getenv("IPC_REPLACEMENT_HELPER") != "1" {
		return
	}
	dir, syncDir := os.Getenv("IPC_REPLACEMENT_DIR"), os.Getenv("IPC_REPLACEMENT_SYNC")
	if os.Getenv("IPC_REPLACEMENT_USE_XDG") == "1" {
		dir = appdir.Config()
	}
	o, err := acquireOwner(dir, func(context.Context, OwnerCommand) (OwnerReply, error) {
		_ = os.WriteFile(filepath.Join(syncDir, "entered"), nil, 0600)
		for {
			if _, e := os.Stat(filepath.Join(syncDir, "release")); e == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		return OwnerReply{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(syncDir, "ready"), nil, 0600)
	for {
		if _, err := os.Stat(filepath.Join(syncDir, "stop")); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	o.Close()
}

func TestOwnerReplacementCannotStartSecondProcessWhileHandlerRuns(t *testing.T) {
	base, err := os.MkdirTemp(os.Getenv("MAGPIE_TEST_SANDBOX"), "ipc-config-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	t.Setenv("XDG_CONFIG_HOME", base)
	dir, syncDir := filepath.Join(base, "magpie"), filepath.Join(base, "sync")
	if appdir.Config() != dir {
		t.Fatal("XDG configuration path differs")
	}
	if err := os.Mkdir(syncDir, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := proc.Command(os.Args[0], "-test.run=^TestOwnerReplacementProcessHelper$")
	// Different HOME values are valid for two processes using one XDG or
	// portable config. The child home remains inside testenv's sandbox.
	altHome, err := os.MkdirTemp(os.Getenv("MAGPIE_TEST_SANDBOX"), "alt-home-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(altHome)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "HOME=") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "HOME="+altHome, "IPC_REPLACEMENT_HELPER=1", "IPC_REPLACEMENT_DIR="+dir, "IPC_REPLACEMENT_SYNC="+syncDir, "IPC_REPLACEMENT_USE_XDG=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	defer func() {
		if !finished {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	waitFor := func(name string) {
		deadline := time.Now().Add(15 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(syncDir, name)); err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("helper did not signal %s", name)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor("ready")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, e := forwardOwner(ctx, dir, OwnerCommand{Operation: "agent-disconnect", AgentID: "hermes"})
		done <- e
	}()
	waitFor("entered")
	if err := os.Rename(dir, dir+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	second, err := acquireOwner(dir, nil)
	if err == nil {
		second.Close()
		t.Fatal("replacement acquired a second owner while old handler was still running")
	}
	if !errors.Is(err, ErrOwnerExists) {
		t.Fatalf("expected retained stable lock, got %v", err)
	}
	if err := os.WriteFile(filepath.Join(syncDir, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	<-done
	if err := os.WriteFile(filepath.Join(syncDir, "stop"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	finished = true
	second, err = acquireOwner(dir, nil)
	if err != nil {
		t.Fatalf("owner did not become available after drain: %v", err)
	}
	second.Close()
}

func TestOwnerIndependentConfigsInSameParent(t *testing.T) {
	base := t.TempDir()
	first, err := acquireOwner(filepath.Join(base, "config-a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	syncDir := filepath.Join(base, "sync")
	if err := os.Mkdir(syncDir, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := proc.Command(os.Args[0], "-test.run=^TestOwnerReplacementProcessHelper$")
	cmd.Env = append(os.Environ(), "IPC_REPLACEMENT_HELPER=1", "IPC_REPLACEMENT_DIR="+filepath.Join(base, "config-b"), "IPC_REPLACEMENT_SYNC="+syncDir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() { _ = cmd.Process.Kill() }()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			t.Fatalf("independent config's owner exited before ready: %v", err)
		case <-deadline.C:
			t.Fatal("independent config's owner did not become ready")
		case <-tick.C:
			if _, err := os.Stat(filepath.Join(syncDir, "ready")); err == nil {
				if err := os.WriteFile(filepath.Join(syncDir, "stop"), nil, 0600); err != nil {
					t.Fatal(err)
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				return
			}
		}
	}
}

func TestStableOwnerRejectsWritableAncestor(t *testing.T) {
	base := t.TempDir()
	ancestor := filepath.Join(base, "writable")
	parent := filepath.Join(ancestor, "parent")
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ancestor, 0775); err != nil {
		t.Fatal(err)
	}
	if f, err := acquireStableOwner(filepath.Join(parent, "config")); err == nil {
		releaseStableOwner(f)
		t.Fatal("group-writable lock ancestor accepted")
	}
}

func TestStableOwnerIgnoresMissingHomeAndPreservesParent(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0775); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", "")
	f, err := acquireStableOwner(filepath.Join(parent, "config"))
	if err != nil {
		t.Fatal(err)
	}
	defer releaseStableOwner(f)
	info, err := os.Stat(parent)
	if err != nil || info.Mode().Perm() != 0775 {
		t.Fatal("parent permissions changed")
	}
}

func TestOwnerPortableWithUnavailableHome(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("HOME", filepath.Join(parent, "does-not-exist"))
	if err := os.WriteFile(filepath.Join(parent, ".portable"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	appdir.UseExecutable(filepath.Join(parent, "magpie"))
	defer appdir.UseExecutable("")
	dir := appdir.Config()
	if dir != filepath.Join(parent, "data") {
		t.Fatalf("portable config resolved to %s", dir)
	}
	o, err := acquireOwner(dir, nil)
	if err != nil {
		t.Fatalf("portable owner depended on HOME: %v", err)
	}
	defer o.Close()
	if _, err := os.Stat(filepath.Join(parent, "does-not-exist")); !os.IsNotExist(err) {
		t.Fatal("owner created or accessed missing HOME")
	}
}
