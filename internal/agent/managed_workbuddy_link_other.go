//go:build !windows

package agent

import (
	"errors"
	"os"
	"syscall"
)

func managedWorkBuddySingleLink(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return ErrManagedInvalid
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Nlink != 1 {
		return ErrManagedInvalid
	}
	return nil
}
