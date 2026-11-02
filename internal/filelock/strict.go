package filelock

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// LockContext takes both the in-process mutex and Path's system lock, or
// returns an error holding neither. The context and wait bound the total
// wait for both locks. Unlike Lock it never continues without either lock.
// Unlock must be called only after a successful return. Locked is called
// only with both locks held, as with Lock.
func (m *Mutex) LockContext(ctx context.Context, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if m.mu.TryLock() {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-poll.C:
		}
	}
	unlock, err := lockContext(ctx, m.Path(), TryLock)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	m.unlock = unlock
	if m.Locked != nil {
		m.Locked()
	}
	return nil
}

// try is TryLock in production; passing it explicitly lets tests exercise
// filesystems that reject locking without requiring an NFS mount.
func lockContext(ctx context.Context, path string, try func(*os.File) (bool, error)) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if errors.Is(err, fs.ErrNotExist) {
		if err = mkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create lock folder: %w", err)
		}
		f, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	}
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	held := false
	defer func() {
		if !held {
			f.Close()
		}
	}()
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		got, err := try(f)
		if err != nil {
			return nil, fmt.Errorf("take file lock: %w", err)
		}
		if got {
			if err = ctx.Err(); err != nil {
				Unlock(f)
				return nil, err
			}
			held = true
			return func() { Unlock(f); f.Close() }, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-poll.C:
		}
	}
}
