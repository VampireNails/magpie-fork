//go:build linux

package gui

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

func listenProtectedUnix(path string) (net.Listener, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("remote socket requires a clean absolute path")
	}
	dir := filepath.Dir(path)
	if err := socketParents(filepath.Dir(dir)); err != nil {
		return nil, err
	}
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}
	i, err := os.Lstat(dir)
	if err != nil || !privateSocketPath(i, os.ModeDir, 0700) {
		return nil, errors.New("remote socket directory must be owned by this user, mode 0700, and not a link")
	}
	// Keep one owner for this path for the listener's entire lifetime. This
	// is a separate lifecycle lock, unrelated to the credential store lock.
	fd, err := syscall.Open(filepath.Join(dir, ".magpie-listener.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, errors.New("cannot open remote socket lifecycle lock")
	}
	lock := os.NewFile(uintptr(fd), "remote socket lock")
	i, err = lock.Stat()
	if err != nil || !privateSocketPath(i, 0, 0600) {
		lock.Close()
		return nil, errors.New("remote socket lifecycle lock must be a private owned regular file")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("remote socket is already owned by another listener")
	}
	ok := false
	defer func() {
		if !ok {
			lock.Close()
		}
	}()
	if before, err := os.Lstat(path); err == nil {
		if !privateSocketPath(before, os.ModeSocket, 0600) {
			return nil, errors.New("remote socket path is not a private owned socket; leaving it untouched")
		}
		var dev, ino uint64
		st := before.Sys().(*syscall.Stat_t)
		if _, err := fmt.Fscan(lock, &dev, &ino); err != nil || dev != uint64(st.Dev) || ino != st.Ino {
			return nil, errors.New("remote socket was not created by this listener; leaving it untouched")
		}
		c, err := net.DialTimeout("unix", path, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return nil, errors.New("remote socket already has an active listener")
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			return nil, errors.New("remote socket liveness is unknown; leaving it untouched")
		}
		after, err := os.Lstat(path)
		if err != nil || !os.SameFile(before, after) {
			return nil, errors.New("remote socket changed during stale check; leaving it untouched")
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, errors.New("cannot inspect remote socket path")
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	// net's default unlink-on-close could remove a path somebody replaced.
	ln.SetUnlinkOnClose(false)
	i, err = os.Lstat(path)
	if err != nil {
		ln.Close()
		return nil, err
	}
	l := &protectedUnix{UnixListener: ln, path: path, info: i, lock: lock}
	if err := os.Chmod(path, 0600); err != nil {
		l.Close()
		return nil, err
	}
	st := i.Sys().(*syscall.Stat_t)
	if err := lock.Truncate(0); err != nil {
		l.Close()
		return nil, err
	}
	if _, err := lock.Seek(0, 0); err != nil {
		l.Close()
		return nil, err
	}
	if _, err := fmt.Fprintf(lock, "%d %d\n", uint64(st.Dev), st.Ino); err != nil {
		l.Close()
		return nil, err
	}
	if err := lock.Sync(); err != nil {
		l.Close()
		return nil, err
	}
	ok = true
	return l, nil
}

// Ancestors must not be links or writable by a different unprivileged
// user. Root's sticky /tmp is safe for an owned child directory.
func socketParents(dir string) error {
	for {
		i, err := os.Lstat(dir)
		if err != nil {
			return errors.New("cannot inspect remote socket directory ancestors")
		}
		st, owned := i.Sys().(*syscall.Stat_t)
		if !i.IsDir() || !owned || (st.Uid != 0 && st.Uid != uint32(os.Geteuid())) || (i.Mode().Perm()&0022 != 0 && !(st.Uid == 0 && i.Mode()&os.ModeSticky != 0)) {
			return fmt.Errorf("remote socket ancestor %s must be an owned non-writable directory, not a link", dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

func privateSocketPath(i os.FileInfo, kind, perm os.FileMode) bool {
	if i == nil {
		return false
	}
	st, ok := i.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Geteuid()) && i.Mode().Type() == kind && i.Mode().Perm() == perm && (kind == os.ModeDir || st.Nlink == 1)
}

type protectedUnix struct {
	*net.UnixListener
	path string
	info os.FileInfo
	lock *os.File
	once sync.Once
	err  error
}

func (l *protectedUnix) Close() error {
	l.once.Do(func() {
		l.err = l.UnixListener.Close()
		if i, err := os.Lstat(l.path); err == nil && os.SameFile(l.info, i) {
			if err := os.Remove(l.path); err != nil && l.err == nil {
				l.err = err
			}
		}
		l.lock.Close()
	})
	return l.err
}
