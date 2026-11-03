package cli

import (
	"context"
	"errors"
	"github.com/yetone/magpie/internal/agent"
	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/remotejoin"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteOwnerCommand(t *testing.T) {
	cases := []struct {
		request remoteRequest
		want    remotejoin.OwnerCommand
		forward bool
	}{
		{remoteRequest{operation: "connect", serverURL: "https://node.example:9443"}, remotejoin.OwnerCommand{Operation: "connect", ServerURL: "https://node.example:9443"}, true},
		{remoteRequest{operation: "status", json: true}, remotejoin.OwnerCommand{Operation: "status"}, true},
		{remoteRequest{operation: "refresh"}, remotejoin.OwnerCommand{Operation: "refresh"}, true},
		{remoteRequest{operation: "disconnect"}, remotejoin.OwnerCommand{Operation: "disconnect"}, true},
		{remoteRequest{operation: "manage", noOpen: true}, remotejoin.OwnerCommand{Operation: "manage"}, true},
		{remoteRequest{operation: "run"}, remotejoin.OwnerCommand{}, false},
	}
	for _, c := range cases {
		got, forward := remoteOwnerCommand(c.request)
		if got != c.want || forward != c.forward {
			t.Errorf("%s: got %+v, %v", c.request.operation, got, forward)
		}
	}
}

func TestRemoteForwardsUnknownOnce(t *testing.T) {
	previous := forwardRemote
	defer func() { forwardRemote = previous }()
	calls := 0
	forwardRemote = func(_ context.Context, c remotejoin.OwnerCommand) (remotejoin.OwnerReply, error) {
		calls++
		return remotejoin.OwnerReply{}, remotejoin.ErrOwnerOutcomeUnknown
	}
	err := remoteCmd([]string{"refresh"})
	if calls != 1 || !errors.Is(err, remotejoin.ErrOwnerOutcomeUnknown) {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
}

func TestManagedAgentFailureDoesNotWriteOrRetry(t *testing.T) {
	cliHome(t)
	dir := appdir.Config()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "remote-connection.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	path := writeHomeFile(t, filepath.Join(".workbuddy", "models.json"), "[]")
	previous := forwardRemote
	defer func() { forwardRemote = previous }()
	calls := 0
	forwardRemote = func(_ context.Context, c remotejoin.OwnerCommand) (remotejoin.OwnerReply, error) {
		calls++
		if c.Operation != "agent-default" {
			t.Errorf("operation %s", c.Operation)
		}
		return remotejoin.OwnerReply{}, remotejoin.ErrOwnerOutcomeUnknown
	}
	err := run([]string{"workbuddy", "default"})
	if calls != 1 || !errors.Is(err, remotejoin.ErrOwnerOutcomeUnknown) {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
	b, e := os.ReadFile(path)
	if e != nil || string(b) != "[]" {
		t.Fatalf("agent config changed: %q, %v", b, e)
	}
}

func TestRemoteConnectRejectsSecretURLBeforeForward(t *testing.T) {
	prev := forwardRemote
	defer func() { forwardRemote = prev }()
	calls := 0
	forwardRemote = func(context.Context, remotejoin.OwnerCommand) (remotejoin.OwnerReply, error) {
		calls++
		return remotejoin.OwnerReply{}, nil
	}
	err := remoteCmd([]string{"connect", "https://user:secret@example.test/"})
	if err == nil || calls != 0 || strings.Contains(err.Error(), "secret") {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
}

func TestManagementURLRejectsCredentialsAndQuery(t *testing.T) {
	for _, s := range []string{"https://user:secret@example.test/_magpie/remote/v1/browser-login", "https://example.test/_magpie/remote/v1/browser-login?key=secret", "https://example.test/other"} {
		if cleanManagementURL(s) {
			t.Error("unsafe management URL accepted")
		}
	}
	if !cleanManagementURL("https://example.test:9443/_magpie/remote/v1/browser-login") {
		t.Error("valid URL rejected")
	}
}

func TestManagedAgentRejectsUnlistedValueBeforeIPC(t *testing.T) {
	cliHome(t)
	if err := os.MkdirAll(appdir.Config(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appdir.Config(), "remote-connection.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := agent.Find("workbuddy")
	if err != nil {
		t.Fatal(err)
	}
	prev := forwardRemote
	defer func() { forwardRemote = prev }()
	calls := 0
	forwardRemote = func(context.Context, remotejoin.OwnerCommand) (remotejoin.OwnerReply, error) {
		calls++
		return remotejoin.OwnerReply{}, nil
	}
	handled, err := forwardManaged(a, "agent-pick", "provider", "arbitrary-config-text")
	if !handled || err == nil || calls != 0 {
		t.Fatalf("handled=%v calls=%d error=%v", handled, calls, err)
	}
}

func TestRemoteAgentOperationRejectsUnknown(t *testing.T) {
	cliHome(t)
	_, err := remoteAgentOperation(context.Background(), remotejoin.OwnerCommand{Operation: "arbitrary", AgentID: "hermes"})
	if err == nil {
		t.Fatal("unknown operation accepted")
	}
}

func TestRemoteAgentChoiceRequiresKnownOption(t *testing.T) {
	cliHome(t)
	if remoteAgentChoice(context.Background(), "workbuddy", "provider", "arbitrary-config") {
		t.Fatal("unknown value accepted")
	}
	if !remoteAgentChoice(context.Background(), "workbuddy", "provider", "magpie") {
		t.Fatal("magpie provider rejected")
	}
	if remoteAgentChoice(context.Background(), "workbuddy-ai", "provider", "magpie") {
		t.Fatal("unmanaged agent accepted")
	}
}

func TestRemoteExitWaitsForRecovery(t *testing.T) {
	attempts, waits := 0, 0
	err := finishRemoteWith(func() error {
		attempts++
		if attempts < 3 {
			return errors.New("restore_failed")
		}
		return nil
	}, errors.New("restore_failed"), func() { waits++ })
	if err != nil || attempts != 3 || waits != 2 {
		t.Fatalf("err=%v attempts=%d waits=%d", err, attempts, waits)
	}
}

func TestRemoteStartupFailureIsReturned(t *testing.T) {
	startup := errors.New("owner_exists")
	err := finishRemoteWith(func() error { return nil }, startup, func() { t.Error("unexpected retry") })
	if !errors.Is(err, startup) {
		t.Fatalf("got %v", err)
	}
}

func TestRemoteStatusShowsLiveStateAndSync(t *testing.T) {
	prev := forwardRemote
	defer func() { forwardRemote = prev }()
	forwardRemote = func(context.Context, remotejoin.OwnerCommand) (remotejoin.OwnerReply, error) {
		return remotejoin.OwnerReply{State: "connected", Status: &remotejoin.RunnerStatus{ClientStatus: remotejoin.ClientStatus{State: "connected", LastErrorCode: "catalog_unavailable"}, Owner: true, Connected: true}}, nil
	}
	out, err := printed(t, func() error { return remoteCmd([]string{"status"}) })
	if err != nil || !strings.Contains(out, "connected") || !strings.Contains(out, "catalog_unavailable") {
		t.Fatalf("status missing details: %q %v", out, err)
	}
}
