//go:build !windows

package remotejoin

import (
	"os"
	"syscall"
)

func ownerSharingConflict(error) bool { return false }

func ownerUnixIdentity(info os.FileInfo) bool {
	s, ok := info.Sys().(*syscall.Stat_t)
	return ok && s.Uid == uint32(os.Geteuid()) && (info.IsDir() || s.Nlink == 1)
}
func protectOwnerDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.IsDir() || !ownerUnixIdentity(info) {
		return ErrOwnerUnavailable
	}
	return f.Chmod(0700)
}
func protectOwnerFile(f *os.File) error {
	info, err := f.Stat()
	if err != nil || !ownerUnixIdentity(info) || info.Mode().Perm()&0077 != 0 {
		return ErrOwnerUnavailable
	}
	return nil
}
