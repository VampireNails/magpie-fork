package remotejoin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/proc"
)

func TestOwnerSecondOwnerRefusedAndFreshForwardExecutes(t *testing.T) {
	dir := t.TempDir()
	calls := make(chan OwnerCommand, 1)
	owner, err := acquireOwner(dir, func(ctx context.Context, c OwnerCommand) (OwnerReply, error) {
		calls <- c
		return OwnerReply{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if second, e := acquireOwner(dir, nil); e == nil {
		second.Close()
		t.Fatal("second owner accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, e := forwardOwner(ctx, dir, OwnerCommand{Operation: "agent-apply", AgentID: "hermes", Key: "model", Value: "test-model"})
	if e != nil || got.Connection != nil {
		t.Fatal("fresh owner request was not executed")
	}
	select {
	case c := <-calls:
		if c.Key != "model" || c.Value != "test-model" {
			t.Fatal("operation lost")
		}
	default:
		t.Fatal("forward returned fake state without executing")
	}
}

func TestOwnerCrossProcessHelper(t *testing.T) {
	if os.Getenv("S9_OWNER_HELPER") != "1" {
		return
	}
	dir := os.Getenv("S9_OWNER_DIR")
	o, err := acquireOwner(dir, func(_ context.Context, c OwnerCommand) (OwnerReply, error) {
		if c.Operation == "agent-connect-how" {
			return OwnerReply{Connection: &OwnerConnection{How: "first", Field: "model", Value: "fixture-model"}}, nil
		}
		return OwnerReply{State: "connected"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	os.WriteFile(filepath.Join(dir, "helper-ready"), []byte("ready"), 0600)
	for {
		time.Sleep(time.Second)
	}
}

func TestOwnerCrossProcessForwardAndCrash(t *testing.T) {
	dir := t.TempDir()
	cmd := proc.Command(os.Args[0], "-test.run=^TestOwnerCrossProcessHelper$")
	cmd.Env = append(os.Environ(), "S9_OWNER_HELPER=1", "S9_OWNER_DIR="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "helper-ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owner did not start")
		}
		time.Sleep(25 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := forwardOwner(ctx, dir, OwnerCommand{Operation: "agent-connect-how", AgentID: "hermes"})
	if err != nil || got.Connection == nil || got.Connection.Field != "model" || got.Connection.Value != "fixture-model" {
		t.Fatalf("owner did not execute operation and return its choice: error=%v reply=%+v", err, got.Connection)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	_, err = forwardOwner(ctx, dir, OwnerCommand{Operation: "status"})
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatal("crashed owner appeared alive")
	}
	other, err := acquireOwner(dir, nil)
	if err != nil {
		t.Fatal("new owner could not acquire stale lock")
	}
	other.Close()
}

func TestOwnerOldDiscoveryAndLockFileAreNotLiveness(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "remote-owner.lock"), nil, 0600)
	os.WriteFile(filepath.Join(dir, "remote-owner.json"), []byte(`{"generation":"11111111111111111111111111111111"}`), 0600)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := forwardOwner(ctx, dir, OwnerCommand{Operation: "status"}); !errors.Is(err, ErrOwnerUnavailable) {
		t.Fatal("stale owner file counted as live")
	}
}

func TestOwnerRejectsUnboundedOrUnknownCommands(t *testing.T) {
	for _, c := range []OwnerCommand{{Operation: "shell", Value: "rm"}, {Operation: "agent-apply", AgentID: "other"}, {Operation: "status", Value: "secret"}, {Operation: "agent-pick", AgentID: "hermes", Key: "api_key", Value: "secret"}, {Operation: "agent-pick", AgentID: "workbuddy", Key: "model", Value: "x"}} {
		if validOwnerCommand(c) {
			t.Fatal("unknown command accepted")
		}
	}
}
