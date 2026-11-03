package remotejoin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOwnerRejectsSymlinkLock(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "remote-owner.lock")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	o, err := acquireOwner(dir, func(context.Context, OwnerCommand) (OwnerReply, error) { return OwnerReply{}, nil })
	if err == nil {
		o.Close()
		t.Fatal("owner accepted symlink lock")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "unchanged" {
		t.Fatal("outside file modified")
	}
}

func TestOwnerRejectsReplacedLockIdentity(t *testing.T) {
	dir := t.TempDir()
	o, err := acquireOwner(dir, func(context.Context, OwnerCommand) (OwnerReply, error) { return OwnerReply{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	path := filepath.Join(dir, "remote-owner.lock")
	if err := os.Rename(path, path+".old"); err != nil {
		t.Skipf("platform prevents replacement: %v", err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if o.validIdentity() {
		t.Fatal("owner accepted replaced lock identity")
	}
}

func TestOwnerAnchoredReadRejectsOutsideSymlink(t *testing.T) {
	dir := t.TempDir()
	d, err := openOwnerDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.root.Close()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("canary-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "reply")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := d.read("reply", 8192); err == nil {
		t.Fatal("outside symlink read accepted")
	}
}

func TestOwnerMailboxOpenUsesHeldConfigIdentity(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "config")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	d, err := openOwnerDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.root.Close()
	const name = "remote-owner-11111111111111111111111111111111"
	if err := d.root.Mkdir(name, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, name, "marker"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+"-old"); err != nil {
		t.Skipf("platform prevents directory rename: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(path, name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, name, "marker"), []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := openOwnerMailbox(d, name)
	if err != nil {
		t.Fatal(err)
	}
	defer m.root.Close()
	data, err := m.read("marker", 64)
	if err != nil || string(data) != "original" {
		t.Fatalf("mailbox came from replacement directory: %q, %v", data, err)
	}
}

func TestOwnerStatusRejectsSecretProjection(t *testing.T) {
	for _, field := range []string{"provider", "device", "mode", "error"} {
		t.Run(field, func(t *testing.T) {
			s := RunnerStatus{ClientStatus: ClientStatus{State: "unconfigured"}, Owner: true}
			switch field {
			case "provider":
				s.ProviderID = "canary-secret"
			case "device":
				s.DeviceID = "canary-secret"
			case "mode":
				s.ConnectionMode = "canary-secret"
			case "error":
				s.LastErrorCode = "canary-secret"
			}
			if validOwnerReply(OwnerCommand{Operation: "status"}, OwnerReply{State: s.State, Status: &s}) {
				t.Fatal("status accepted arbitrary secret projection")
			}
		})
	}
}

func TestOwnerReleaseBeforeDrainKeepsOwnership(t *testing.T) {
	dir := t.TempDir()
	o, err := acquireOwner(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	o.release()
	second, err := acquireOwner(dir, nil)
	if err == nil {
		second.Close()
		t.Fatal("release before drain gave up ownership")
	}
}
