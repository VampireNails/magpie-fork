package provider

import (
	"context"
	"fmt"
	"github.com/yetone/magpie/internal/catalog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestRemoteBindingDoesNotNotifyAgentSync(t *testing.T) {
	azureHome(t)
	called := 0
	old := catalog.Changed
	catalog.Changed = func() { called++ }
	defer func() { catalog.Changed = old }()
	if err := BindRemote("remote-fixed", "https://node.tailnet.ts.net", "key"); err != nil {
		t.Fatal(err)
	}
	if called != 0 {
		t.Fatal("binding notified agent synchronization")
	}
}

func TestRemoteBindingNativeRefreshKeepsAllAndDoesNotNotify(t *testing.T) {
	azureHome(t)
	agentConfig := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(agentConfig, []byte("original agent settings"), 0600); err != nil {
		t.Fatal(err)
	}
	count := 30
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "secret-canary", 500)
			return
		}
		fmt.Fprint(w, `{"data":[`)
		for i := 0; i < count; i++ {
			if i > 0 {
				fmt.Fprint(w, ",")
			}
			fmt.Fprintf(w, `{"id":"upstream/model-%d"}`, i)
		}
		fmt.Fprint(w, `]}`)
	}))
	defer server.Close()
	old := catalog.Changed
	called := 0
	catalog.Changed = func() { called++; _ = os.WriteFile(agentConfig, []byte("rewritten"), 0600) }
	defer func() { catalog.Changed = old }()
	if err := BindRemote("remote-fixed", server.URL, "test-key"); err != nil {
		t.Fatal(err)
	}
	p, err := Find("remote-fixed")
	if err != nil {
		t.Fatal(err)
	}
	if err = p.RefetchRemoteQuiet(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(p.Exposed()); got != 30 {
		t.Fatalf("got %d models", got)
	}
	count = 31
	if err = p.RefetchRemoteQuiet(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(p.Exposed()); got != 31 {
		t.Fatalf("new model missing: %d", got)
	}
	fail = true
	if err = p.RefetchRemoteQuiet(context.Background()); err == nil {
		t.Fatal("failure not reported")
	}
	if got := len(p.Exposed()); got != 31 {
		t.Fatalf("old list lost: %d", got)
	}
	if called != 0 {
		t.Fatal("agent synchronization notified")
	}
	contents, err := os.ReadFile(agentConfig)
	if err != nil || string(contents) != "original agent settings" {
		t.Fatal("agent configuration changed")
	}
}

func TestRemoteBindingListRedirectAndErrorsDoNotLeak(t *testing.T) {
	azureHome(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect forwarded credential") }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer source.Close()
	if err := BindRemote("remote-redirect", source.URL, "secret-canary"); err != nil {
		t.Fatal(err)
	}
	p, err := Find("remote-redirect")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = p.Refetch(context.Background())
	if err == nil || strings.Contains(err.Error(), "secret-canary") {
		t.Fatal("unsafe list redirect or leaked error")
	}
}
