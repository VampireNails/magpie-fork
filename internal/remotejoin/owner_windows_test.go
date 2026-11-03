//go:build windows

package remotejoin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestOwnerResponseReadWaitsForWindowsSharing(t *testing.T) {
	dir := t.TempDir()
	d, err := openOwnerDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.root.Close()
	path := filepath.Join(dir, "response")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.ReadFile(path); err == nil {
		windows.CloseHandle(h)
		t.Fatal("fixture did not block an ordinary read")
	}
	result := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _, err := d.readContext(ctx, "response", 8192); result <- err }()
	select {
	case err := <-result:
		windows.CloseHandle(h)
		t.Fatalf("read ended before release: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err = windows.CloseHandle(h); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-result:
		if err != nil {
			t.Fatal("read failed after release")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("read did not finish")
	}
}

func TestOwnerMailboxWindowsACLRejectsOtherUsers(t *testing.T) {
	dir := t.TempDir()
	d, err := openOwnerDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.root.Close()
	f, err := d.open("fixture", os.O_RDWR|os.O_CREATE|os.O_EXCL)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := protectOwnerFile(f); err != nil {
		t.Fatalf("private inherited ACL rejected: %v", err)
	}
	public, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := public.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(filepath.Join(dir, "fixture"), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if err := protectOwnerFile(f); err == nil {
		t.Fatal("Everyone ACL accepted")
	}
}

func TestOwnerWindowsSharingReadIsCancellable(t *testing.T) {
	dir := t.TempDir()
	d, err := openOwnerDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.root.Close()
	if err := d.write("response", []byte("fixture")); err != nil {
		t.Fatal(err)
	}
	p, err := windows.UTF16PtrFromString(filepath.Join(dir, "response"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := d.readContext(ctx, "response", 8192); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("sharing read returned before cancellation: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("sharing read ignored cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("sharing read did not cancel")
	}
}
