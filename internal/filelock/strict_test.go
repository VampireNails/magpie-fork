package filelock

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStrictLockUnsupported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")
	var handle *os.File
	unlock, err := lockContext(context.Background(), path, func(f *os.File) (bool, error) {
		handle = f
		return false, fs.ErrInvalid
	})
	if !errors.Is(err, fs.ErrInvalid) || unlock != nil {
		t.Fatal("unsupported locking reported success")
	}
	if _, err := handle.Write([]byte("x")); err == nil {
		t.Fatal("failed locking leaked its file")
	}
	// An error must not unlink the shared lock file; that would split locks.
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestStrictMutexFailureReleasesLocalLock(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "missing", "state.lock")
	called := 0
	m := Mutex{Path: func() string { return path }, Locked: func() { called++ }}
	if err := os.WriteFile(filepath.Join(root, "missing"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.LockContext(context.Background(), time.Second); err == nil {
		t.Fatal("lock folder creation failure reported success")
	}
	if called != 0 {
		t.Fatal("Locked ran without the file lock")
	}
	path = filepath.Join(root, "working", "state.lock")
	if err := m.LockContext(context.Background(), time.Second); err != nil {
		t.Fatal("failure left local mutex held", err)
	}
	if called != 1 {
		t.Error("Locked was not called after acquiring both locks")
	}
	m.Unlock()
	if unlock, held := Lock(path, time.Second); !held {
		t.Fatal("Unlock left the system lock held")
	} else {
		unlock()
	}
}

func TestStrictMutexCancellationAndTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")
	m := Mutex{Path: func() string { return path }}
	if err := m.LockContext(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	// The real local mutex stays held while a second call times out.
	if err := m.LockContext(context.Background(), 30*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Error("local mutex wait ignored its timeout")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.LockContext(ctx, time.Second); !errors.Is(err, context.Canceled) {
		t.Error("local mutex wait ignored cancellation")
	}
	m.Unlock()
	if err := m.LockContext(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	m.Unlock()
}

func TestStrictLockCanceledOnAcquisition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")
	ctx, cancel := context.WithCancel(context.Background())
	unlock, err := lockContext(ctx, path, func(f *os.File) (bool, error) {
		got, e := TryLock(f)
		cancel()
		return got, e
	})
	if !errors.Is(err, context.Canceled) || unlock != nil {
		t.Fatal("canceled acquisition reported success")
	}
	if release, held := Lock(path, time.Second); !held {
		t.Fatal("canceled acquisition leaked its system lock")
	} else {
		release()
	}
}
