//go:build linux

package gui

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoteServerSocketInterruptedInitialization(t *testing.T) {
	if point := os.Getenv("MAGPIE_SOCKET_TEST_EXIT"); point != "" {
		socketCheckpoint = func(p string) error {
			if p == point {
				os.Exit(31)
			}
			return nil
		}
		listenProtectedUnix(os.Getenv("MAGPIE_SOCKET_TEST_PATH"))
		os.Exit(32)
	}
	for _, point := range []string{"lock", "bind", "marker-write", "marker-sync", "published"} {
		t.Run(point, func(t *testing.T) {
			p := socketPath(t)
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			proc, err := os.StartProcess(exe, []string{exe, "-test.run=^TestRemoteServerSocketInterruptedInitialization$"}, &os.ProcAttr{Env: append(os.Environ(), "MAGPIE_SOCKET_TEST_EXIT="+point, "MAGPIE_SOCKET_TEST_PATH="+p)})
			if err != nil {
				t.Fatal(err)
			}
			state, err := proc.Wait()
			if err != nil || state.ExitCode() != 31 {
				t.Fatal("child did not exit at checkpoint", err)
			}
			l, err := listenProtectedUnix(p)
			if err != nil {
				t.Fatalf("restart after %s: %v", point, err)
			}
			l.Close()
		})
	}
}

func TestRemoteServerSocketMarkerFailureCanRestart(t *testing.T) {
	for _, point := range []string{"marker-write", "marker-sync"} {
		t.Run(point, func(t *testing.T) {
			p := socketPath(t)
			socketCheckpoint = func(p string) error {
				if p == point {
					return errors.New("injected marker failure")
				}
				return nil
			}
			_, err := listenProtectedUnix(p)
			socketCheckpoint = func(string) error { return nil }
			if err == nil {
				t.Fatal("expected marker failure")
			}
			l, err := listenProtectedUnix(p)
			if err != nil {
				t.Fatal("restart after marker failure:", err)
			}
			l.Close()
		})
	}
}

func socketPath(t *testing.T) string {
	t.Helper()
	// Go's sandbox HOME can exceed sockaddr_un's path limit.
	d, err := os.MkdirTemp("/tmp", "magpie-socket-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return filepath.Join(d, "private", "serve.sock")
}

func TestRemoteServerSocketCreatesPrivateDirectory(t *testing.T) {
	p := socketPath(t)
	l, err := listenProtectedUnix(p)
	if err != nil {
		t.Fatal("protected listener did not create its private directory:", err)
	}
	defer l.Close()
	for path, want := range map[string]os.FileMode{filepath.Dir(p): 0700, p: 0600} {
		i, err := os.Lstat(path)
		if err != nil || i.Mode().Perm() != want {
			t.Fatal("listener path permissions are not private")
		}
	}
	if other, err := listenProtectedUnix(p); err == nil {
		other.Close()
		t.Fatal("a second listener replaced an active socket")
	}
}

func TestRemoteServerSocketNeverDeletesUnknownPaths(t *testing.T) {
	for _, kind := range []string{"file", "link", "directory", "public-directory", "parent-link"} {
		t.Run(kind, func(t *testing.T) {
			p := socketPath(t)
			if err := os.Mkdir(filepath.Dir(p), 0700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "file":
				os.WriteFile(p, []byte("keep"), 0600)
			case "link":
				os.Symlink("missing-target", p)
			case "directory":
				os.Mkdir(p, 0700)
			case "public-directory":
				os.Chmod(filepath.Dir(p), 0755)
			case "parent-link":
				os.Rename(filepath.Dir(p), filepath.Dir(p)+"-real")
				os.Symlink(filepath.Dir(p)+"-real", filepath.Dir(p))
			}
			before, _ := os.Lstat(p)
			l, err := listenProtectedUnix(p)
			if err == nil {
				l.Close()
				t.Fatal("listener accepted an unsafe or unknown path")
			}
			if before != nil {
				after, err := os.Lstat(p)
				if err != nil || !os.SameFile(before, after) {
					t.Fatal("listener deleted or replaced an unknown path")
				}
			}
		})
	}
}

func TestRemoteServerSocketRecoversOnlyPrivateStaleSocket(t *testing.T) {
	p := socketPath(t)
	os.Mkdir(filepath.Dir(p), 0700)
	old, err := listenProtectedUnix(p)
	if err != nil {
		t.Fatal(err)
	}
	crashed := old.(*protectedUnix)
	crashed.UnixListener.Close()
	crashed.lock.Close()
	l, err := listenProtectedUnix(p)
	if err != nil {
		t.Fatal("private crashed listener was not recoverable:", err)
	}
	l.Close()
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Fatal("closed listener left its own socket")
	}
}

func TestRemoteServerSocketPreservesUnknownStaleSocket(t *testing.T) {
	p := socketPath(t)
	os.Mkdir(filepath.Dir(p), 0700)
	old, err := net.ListenUnix("unix", &net.UnixAddr{Name: p, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	old.SetUnlinkOnClose(false)
	os.Chmod(p, 0600)
	old.Close()
	before, _ := os.Lstat(p)
	l, err := listenProtectedUnix(p)
	if err == nil {
		l.Close()
		t.Fatal("unrecognized stale socket was replaced")
	}
	after, err := os.Lstat(p)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("unrecognized socket was removed")
	}
}

func TestRemoteServerSocketPreservesUnknownLockFile(t *testing.T) {
	p := socketPath(t)
	os.Mkdir(filepath.Dir(p), 0700)
	lock := filepath.Join(filepath.Dir(p), ".magpie-listener.lock")
	os.WriteFile(lock, []byte("other application data"), 0600)
	l, err := listenProtectedUnix(p)
	if err == nil {
		l.Close()
		t.Fatal("unknown lock file was accepted")
	}
	b, err := os.ReadFile(lock)
	if err != nil || string(b) != "other application data" {
		t.Fatal("unknown lock file was modified")
	}
}

func TestRemoteServerSocketRestartsAfterUnknownPathRemoved(t *testing.T) {
	p := socketPath(t)
	if err := os.Mkdir(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("unknown"), 0600); err != nil {
		t.Fatal(err)
	}
	if l, err := listenProtectedUnix(p); err == nil {
		l.Close()
		t.Fatal("accepted unknown path")
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	l, err := listenProtectedUnix(p)
	if err != nil {
		t.Fatalf("restart after removing unknown path: %v", err)
	}
	l.Close()
}

func TestRemoteServerSocketClosePreservesReplacement(t *testing.T) {
	p := socketPath(t)
	os.Mkdir(filepath.Dir(p), 0700)
	l, err := listenProtectedUnix(p)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(p)
	os.WriteFile(p, []byte("unrelated"), 0600)
	l.Close()
	if b, err := os.ReadFile(p); err != nil || string(b) != "unrelated" {
		t.Fatal("close removed a replacement path")
	}
}
