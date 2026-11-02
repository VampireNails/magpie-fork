package provider

import (
	"os"
	"testing"
)

func TestRemoteBindingKeepsIdentityAndOtherProviders(t *testing.T) {
	azureHome(t)
	if err := Save(Provider{ID: "other", Chat: "https://other.invalid/v1", Key: "other-key"}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"first-key", "replacement-key"} {
		if err := BindRemote("remote-fixed", "https://node.tailnet.ts.net:9443", key); err != nil {
			t.Fatal(err)
		}
	}
	p, err := Find("remote-fixed")
	if err != nil {
		t.Fatal(err)
	}
	if p.Key != "replacement-key" || !p.IsRemoteMagpie() || len(p.Models) != 0 || p.BestInFamily || p.RefreshSchedule() != "10m" {
		t.Fatal("binding lost native Remote semantics")
	}
	q, err := Find("other")
	if err != nil || q.Key != "other-key" {
		t.Fatal("other provider changed")
	}
	if len(load().Providers) != 2 {
		t.Fatal("duplicate provider")
	}
}

func TestRemoteBindingRejectsCollisionAndBrokenStore(t *testing.T) {
	azureHome(t)
	if err := Save(Provider{ID: "occupied", Chat: "https://other.invalid/v1", Key: "untouched"}); err != nil {
		t.Fatal(err)
	}
	if BindRemote("occupied", "https://node.tailnet.ts.net", "new-key") == nil {
		t.Fatal("overwrote user's provider")
	}
	if err := os.WriteFile(Path(), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if BindRemote("remote-fixed", "https://node.tailnet.ts.net", "new-key") == nil {
		t.Fatal("treated broken store as empty")
	}
	b, _ := os.ReadFile(Path())
	if string(b) != "broken" {
		t.Fatal("overwrote broken store")
	}
}
