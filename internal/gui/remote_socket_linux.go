//go:build linux

package gui

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// socketCheckpoint is replaced by tests to exercise interrupted initialization.
var socketCheckpoint = func(string) error { return nil }

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
	lockPath := filepath.Join(dir, ".magpie-listener.lock")
	// Publish a fully initialized lock atomically. A crash while preparing the
	// temporary file can never leave an empty file at the shared lock path.
	tmp, err := os.CreateTemp(dir, ".magpie-lock-")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	tmpInfo, statErr := tmp.Stat()
	if statErr != nil {
		tmp.Close()
		return nil, statErr
	}
	if _, err = tmp.WriteString("magpie-remote-lock-v2\n"); err == nil {
		err = tmp.Sync()
	}
	if err == nil {
		err = unix.Renameat2(unix.AT_FDCWD, tmpPath, unix.AT_FDCWD, lockPath, unix.RENAME_NOREPLACE)
	}
	tmp.Close()
	if now, e := os.Lstat(tmpPath); e == nil && os.SameFile(tmpInfo, now) {
		os.Remove(tmpPath)
	}
	if err != nil && !os.IsExist(err) {
		return nil, errors.New("cannot create remote socket lifecycle lock")
	}
	fd, err := syscall.Open(lockPath, syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0600)
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
	if current, e := os.Lstat(lockPath); e != nil || !os.SameFile(i, current) {
		lock.Close()
		return nil, errors.New("remote socket lifecycle lock changed")
	}
	ok := false
	defer func() {
		if !ok {
			lock.Close()
		}
	}()
	if err := socketCheckpoint("lock"); err != nil {
		return nil, err
	}
	content, err := io.ReadAll(io.LimitReader(lock, 257))
	if err != nil || len(content) > 256 {
		return nil, errors.New("cannot validate remote socket lifecycle lock")
	}
	lines := strings.Split(string(content), "\n")
	validMarker := func(s string) bool {
		var dev, ino uint64
		var marker, stage, extra string
		n, _ := fmt.Sscan(s, &dev, &ino, &marker, &stage, &extra)
		return marker == "magpie-remote-socket-v1" && (n == 3 || (n == 4 && strings.HasPrefix(stage, ".s-") && filepath.Base(stage) == stage))
	}
	if len(lines) < 2 || (lines[0] != "magpie-remote-lock-v2" && !validMarker(lines[0])) {
		return nil, errors.New("unrecognized remote socket lifecycle lock; leaving it untouched")
	}
	// The first complete line is the stable lock identity. A partial last
	// record is an interrupted write and cannot identify a socket.
	baseLen := len(lines[0]) + 1
	var records []string
	if validMarker(lines[0]) {
		records = append(records, lines[0])
	}
	for _, line := range lines[1 : len(lines)-1] {
		if !validMarker(line) {
			return nil, errors.New("unrecognized remote socket lifecycle lock; leaving it untouched")
		}
		records = append(records, line)
	}
	if before, err := os.Lstat(path); err == nil {
		st := before.Sys().(*syscall.Stat_t)
		if st.Nlink == 2 && st.Uid == uint32(os.Geteuid()) && before.Mode().Type() == os.ModeSocket && before.Mode().Perm() == 0600 {
			for _, record := range records {
				var dev, ino uint64
				var marker, stage string
				if n, _ := fmt.Sscan(record, &dev, &ino, &marker, &stage); n == 4 && dev == uint64(st.Dev) && ino == st.Ino {
					staged := filepath.Join(dir, stage)
					if si, e := os.Lstat(staged); e == nil && os.SameFile(before, si) {
						if e = os.Remove(staged); e != nil {
							return nil, e
						}
					}
				}
			}
			before, err = os.Lstat(path)
			if err != nil {
				return nil, errors.New("cannot inspect remote socket path")
			}
		}
		if !privateSocketPath(before, os.ModeSocket, 0600) {
			return nil, errors.New("remote socket path is not a private owned socket; leaving it untouched")
		}
		var dev, ino uint64
		st = before.Sys().(*syscall.Stat_t)
		var marker string
		matched := false
		for _, record := range records {
			fmt.Sscan(record, &dev, &ino, &marker)
			if dev == uint64(st.Dev) && ino == st.Ino {
				matched = true
			}
		}
		if !matched {
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
	// Bind at an unpredictable staging name. Until the inode is recorded and
	// flushed it is never published at the caller's socket path.
	stage, err := os.CreateTemp(dir, ".s-")
	if err != nil {
		return nil, err
	}
	stagePath := stage.Name()
	stageInfo, err := stage.Stat()
	stage.Close()
	if err != nil {
		return nil, err
	}
	if now, err := os.Lstat(stagePath); err != nil || !os.SameFile(stageInfo, now) {
		return nil, errors.New("remote socket staging path changed")
	}
	if err := os.Remove(stagePath); err != nil {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: stagePath, Net: "unix"})
	if err != nil {
		return nil, err
	}
	// net's default unlink-on-close could remove a path somebody replaced.
	ln.SetUnlinkOnClose(false)
	i, err = os.Lstat(stagePath)
	if err != nil {
		ln.Close()
		return nil, err
	}
	l := &protectedUnix{UnixListener: ln, path: stagePath, info: i, lock: lock}
	if err := socketCheckpoint("bind"); err != nil {
		l.Close()
		return nil, err
	}
	if err := os.Chmod(stagePath, 0600); err != nil {
		l.Close()
		return nil, err
	}
	st := i.Sys().(*syscall.Stat_t)
	if err := socketCheckpoint("marker-write"); err != nil {
		l.Close()
		return nil, err
	}
	if err := lock.Truncate(int64(baseLen)); err != nil {
		l.Close()
		return nil, err
	}
	if _, err := lock.Seek(int64(baseLen), 0); err != nil {
		l.Close()
		return nil, err
	}
	if _, err := fmt.Fprintf(lock, "%d %d magpie-remote-socket-v1 %s\n", uint64(st.Dev), st.Ino, filepath.Base(stagePath)); err != nil {
		l.Close()
		return nil, err
	}
	if err := socketCheckpoint("marker-sync"); err != nil {
		l.Close()
		return nil, err
	}
	if err := lock.Sync(); err != nil {
		l.Close()
		return nil, err
	}
	if now, err := os.Lstat(stagePath); err != nil || !os.SameFile(i, now) {
		l.Close()
		return nil, errors.New("remote socket staging path changed")
	}
	if err := os.Link(stagePath, path); err != nil {
		l.Close()
		return nil, err
	}
	if now, err := os.Lstat(path); err != nil || !os.SameFile(i, now) {
		l.Close()
		return nil, errors.New("remote socket path changed during publication")
	}
	if err := socketCheckpoint("published"); err != nil {
		l.Close()
		return nil, err
	}
	if now, err := os.Lstat(stagePath); err != nil || !os.SameFile(i, now) {
		l.Close()
		return nil, errors.New("remote socket staging path changed")
	}
	if err := os.Remove(stagePath); err != nil {
		l.Close()
		return nil, err
	}
	l.path = path
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
