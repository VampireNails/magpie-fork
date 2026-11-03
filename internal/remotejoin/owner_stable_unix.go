//go:build linux

package remotejoin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

// Bind a per-user, per-config abstract socket while holding the parent handle.
// The kernel retains its unique name until the listener closes. Parent identity
// must be protected by
// every ancestor: writable ancestors require sticky semantics, and each path
// component must be owned by the user or root. The parent may be writable by
// the group: such a user can replace config, but cannot replace its parent.
type stableOwner struct {
	parent   *os.File
	listener *net.UnixListener
}

func acquireStableOwner(dir string) (*stableOwner, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, ErrOwnerUnavailable
	}
	parent := filepath.Dir(filepath.Clean(abs))
	for p := parent; ; p = filepath.Dir(p) {
		info, e := os.Lstat(p)
		if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, ErrOwnerUnavailable
		}
		s, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (s.Uid != 0 && s.Uid != uint32(os.Geteuid())) {
			return nil, ErrOwnerUnavailable
		}
		if p != parent && info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0 {
			return nil, ErrOwnerUnavailable
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	before, err := os.Lstat(parent)
	if err != nil {
		return nil, ErrOwnerUnavailable
	}
	f, err := os.Open(parent)
	if err != nil {
		return nil, ErrOwnerUnavailable
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		f.Close()
		return nil, ErrOwnerUnavailable
	}
	sum := sha256.Sum256([]byte(filepath.Clean(abs)))
	address := &net.UnixAddr{Name: fmt.Sprintf("@magpie-owner-%d-%s", os.Geteuid(), hex.EncodeToString(sum[:])), Net: "unix"}
	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		f.Close()
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, ErrOwnerExists
		}
		return nil, ErrOwnerUnavailable
	}
	return &stableOwner{parent: f, listener: listener}, nil
}

func releaseStableOwner(owner *stableOwner) {
	if owner != nil {
		owner.listener.Close()
		owner.parent.Close()
	}
}

func verifyOwnerPath(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ErrOwnerUnavailable
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil || filepath.Clean(resolved) != filepath.Clean(abs) {
		return ErrOwnerUnavailable
	}
	return nil
}
