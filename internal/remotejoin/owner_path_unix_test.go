//go:build !windows

package remotejoin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOwnerMailboxUnixModeAndHardlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	d, err := openOwnerDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.root.Close()
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("directory not made private")
	}
	f, err := d.open("fixture", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Chmod(0644); err != nil {
		t.Fatal(err)
	}
	if protectOwnerFile(f) == nil {
		t.Fatal("public file mode accepted")
	}
	if err := f.Chmod(0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(dir, "fixture"), filepath.Join(t.TempDir(), "alias")); err != nil {
		t.Fatal(err)
	}
	if protectOwnerFile(f) == nil {
		t.Fatal("hard-linked lock accepted")
	}
}
