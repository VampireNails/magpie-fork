package remotejoin

import (
	"context"
	"errors"
	"io"
	"os"
	"time"
)

func (d *ownerDirectory) readContext(ctx context.Context, name string, limit int64) ([]byte, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := d.read(name, limit)
		if !ownerSharingConflict(err) {
			return data, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// ownerDirectory keeps all IPC I/O anchored to the opened directory, even if
// its pathname is renamed. Links and special files are never mailbox objects.
type ownerDirectory struct {
	root *os.Root
	info os.FileInfo
}

func openOwnerDirectory(path string) (*ownerDirectory, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, ErrOwnerUnavailable
	}
	if err := protectOwnerDirectory(path); err != nil {
		return nil, ErrOwnerUnavailable
	}
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, ErrOwnerUnavailable
	}
	after, err := r.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		r.Close()
		return nil, ErrOwnerUnavailable
	}
	return &ownerDirectory{root: r, info: after}, nil
}

// openOwnerMailbox resolves a generation under the already-open config root;
// reopening through the original absolute path could bind to a different root.
func openOwnerMailbox(config *ownerDirectory, name string) (*ownerDirectory, error) {
	before, err := config.root.Lstat(name)
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, ErrOwnerUnavailable
	}
	r, err := config.root.OpenRoot(name)
	if err != nil {
		return nil, ErrOwnerUnavailable
	}
	after, err := r.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		r.Close()
		return nil, ErrOwnerUnavailable
	}
	f, err := r.Open(".")
	if err != nil {
		r.Close()
		return nil, ErrOwnerUnavailable
	}
	err = protectOwnerFile(f)
	f.Close()
	if err != nil {
		r.Close()
		return nil, ErrOwnerUnavailable
	}
	return &ownerDirectory{root: r, info: after}, nil
}

func (d *ownerDirectory) open(name string, flags int) (*os.File, error) {
	before, err := d.root.Lstat(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil && !before.Mode().IsRegular() {
		return nil, ErrOwnerUnavailable
	}
	f, err := d.root.OpenFile(name, flags, 0600)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || (before != nil && !os.SameFile(before, after)) {
		f.Close()
		return nil, ErrOwnerUnavailable
	}
	if err := protectOwnerFile(f); err != nil {
		f.Close()
		return nil, ErrOwnerUnavailable
	}
	return f, nil
}

func (d *ownerDirectory) read(name string, limit int64) ([]byte, error) {
	f, err := d.open(name, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(data)) > limit {
		return nil, ErrOwnerUnavailable
	}
	return data, err
}

func (d *ownerDirectory) write(name string, data []byte) error {
	id, err := ownerID()
	if err != nil {
		return err
	}
	temp := id + ".tmp"
	f, err := d.open(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return err
	}
	defer d.root.Remove(temp)
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return d.root.Rename(temp, name)
}

func (d *ownerDirectory) alive(name string) bool {
	f, err := d.open(name, os.O_RDONLY)
	if err != nil {
		return false
	}
	f.Close()
	return true
}
