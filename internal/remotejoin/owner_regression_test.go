package remotejoin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/proc"
)

func TestOwnerRejectsEmptyForgedResponse(t *testing.T) {
	dir := t.TempDir()
	f, err := os.OpenFile(filepath.Join(dir, "remote-owner.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	locked, err := filelock.TryLock(f)
	if err != nil || !locked {
		t.Fatal("lock failed")
	}
	defer filelock.Unlock(f)
	gen := strings.Repeat("1", 32)
	box := filepath.Join(dir, "remote-owner-"+gen)
	if err := os.Mkdir(box, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "remote-owner.json"), []byte(`{"generation":"`+gen+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := forwardOwner(ctx, dir, OwnerCommand{Operation: "status"}); result <- err }()
	for {
		entries, err := os.ReadDir(box)
		if err != nil {
			t.Fatal(err)
		}
		var requestName string
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".req") {
				requestName = entry.Name()
				break
			}
		}
		if requestName != "" {
			name := strings.TrimSuffix(requestName, ".req") + ".res"
			if err := os.WriteFile(filepath.Join(box, name), []byte(`{}`), 0600); err != nil {
				t.Fatal(err)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("request not published")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if err := <-result; err == nil {
		t.Fatal("empty forged response accepted as success")
	}
}

func TestOwnerExpiredQueuedCommandNeverExecutes(t *testing.T) {
	dir := t.TempDir()
	started := make(chan struct{})
	release := make(chan struct{})
	executed := make(chan struct{}, 1)
	o, err := acquireOwner(dir, func(ctx context.Context, c OwnerCommand) (OwnerReply, error) {
		if c.Operation == "status" {
			close(started)
			<-release
		} else {
			executed <- struct{}{}
		}
		return OwnerReply{State: "connected", Status: &RunnerStatus{ClientStatus: ClientStatus{State: "connected"}, Owner: true, Connected: true}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	first, cancelFirst := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFirst()
	done := make(chan struct{})
	go func() { defer close(done); forwardOwner(first, dir, OwnerCommand{Operation: "status"}) }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err = forwardOwner(ctx, dir, OwnerCommand{Operation: "disconnect"})
	if !errors.Is(err, ErrOwnerOutcomeUnknown) {
		t.Fatalf("expected unknown outcome, got %v", err)
	}
	close(release)
	<-done
	select {
	case <-executed:
		t.Fatal("expired queued command executed")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestOwnerDrainTimeoutRetainsLockUntilHandlerStops(t *testing.T) {
	dir := t.TempDir()
	started := make(chan struct{})
	release := make(chan struct{})
	o, err := acquireOwner(dir, func(ctx context.Context, c OwnerCommand) (OwnerReply, error) {
		close(started)
		<-release // Deliberately ignore cancellation to model a non-cooperative writer.
		return OwnerReply{State: "connected", Status: &RunnerStatus{ClientStatus: ClientStatus{State: "connected"}, Owner: true, Connected: true}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); forwardOwner(ctx, dir, OwnerCommand{Operation: "status"}) }()
	<-started
	drain, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	if err := o.quiesceContext(drain); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain result: %v", err)
	}
	if second, err := acquireOwner(dir, nil); err == nil {
		second.Close()
		t.Fatal("owner lock released while handler could write")
	}
	close(release)
	if err := o.quiesceContext(ctx); err != nil {
		t.Fatal(err)
	}
	o.release()
	<-done
	second, err := acquireOwner(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
}

func TestOwnerRejectsMismatchedResponseIdentity(t *testing.T) {
	for _, field := range []string{"generation", "id", "operation", "unknown"} {
		t.Run(field, func(t *testing.T) {
			dir := t.TempDir()
			f, err := os.OpenFile(filepath.Join(dir, "remote-owner.lock"), os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			ok, err := filelock.TryLock(f)
			if err != nil || !ok {
				t.Fatal("lock failed")
			}
			defer filelock.Unlock(f)
			gen := strings.Repeat("1", 32)
			box := filepath.Join(dir, "remote-owner-"+gen)
			if err := os.Mkdir(box, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "remote-owner.json"), []byte(`{"generation":"`+gen+`"}`), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := forwardOwner(ctx, dir, OwnerCommand{Operation: "status"}); result <- err }()
			for {
				entries, err := os.ReadDir(box)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if !strings.HasSuffix(entry.Name(), ".req") {
						continue
					}
					id := strings.TrimSuffix(entry.Name(), ".req")
					data := map[string]any{"generation": gen, "id": id, "operation": "status", "error": false, "reply": map[string]any{"state": "connected", "status": map[string]any{"state": "connected"}}}
					switch field {
					case "generation":
						data[field] = strings.Repeat("2", 32)
					case "id":
						data[field] = strings.Repeat("3", 32)
					case "operation":
						data[field] = "disconnect"
					case "unknown":
						data[field] = true
					}
					encoded, _ := json.Marshal(data)
					if err := os.WriteFile(filepath.Join(box, id+".res"), encoded, 0600); err != nil {
						t.Fatal(err)
					}
					if err := <-result; !errors.Is(err, ErrOwnerOutcomeUnknown) {
						t.Fatalf("mismatch accepted or wrong result: %v", err)
					}
					return
				}
				select {
				case <-ctx.Done():
					t.Fatal("request not published")
				default:
					time.Sleep(5 * time.Millisecond)
				}
			}
		})
	}
}

func TestOwnerRequestIDReplayDoesNotExecuteTwice(t *testing.T) {
	dir := t.TempDir()
	calls := make(chan struct{}, 2)
	o, err := acquireOwner(dir, func(context.Context, OwnerCommand) (OwnerReply, error) { calls <- struct{}{}; return OwnerReply{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	gen := strings.TrimPrefix(filepath.Base(o.dir), "remote-owner-")
	id := strings.Repeat("a", 32)
	req := ownerRequest{Generation: gen, ID: id, Expires: time.Now().Add(5 * time.Second), Command: OwnerCommand{Operation: "agent-disconnect", AgentID: "hermes"}}
	data, _ := json.Marshal(req)
	if err := os.WriteFile(filepath.Join(o.dir, id+".alive"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(o.dir, id+".req")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-calls:
	case <-time.After(5 * time.Second):
		t.Fatal("first request not executed")
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-calls:
		t.Fatal("replayed request executed")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestOwnerCancelledRequestResponseIsEventuallyRemoved(t *testing.T) {
	dir := t.TempDir()
	started := make(chan struct{})
	release := make(chan struct{})
	o, err := acquireOwner(dir, func(context.Context, OwnerCommand) (OwnerReply, error) {
		close(started)
		<-release
		return OwnerReply{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := forwardOwner(ctx, dir, OwnerCommand{Operation: "agent-disconnect", AgentID: "hermes"})
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, ErrOwnerOutcomeUnknown) {
		t.Fatalf("cancel: %v", err)
	}
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(o.dir)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".res") {
				found = true
			}
		}
		if found {
			t.Fatal("response persisted after caller cancelled")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestOwnerResponseRejectsMissingNestedStatusFields(t *testing.T) {
	c := OwnerCommand{Operation: "status"}
	data := []byte(`{"generation":"11111111111111111111111111111111","id":"22222222222222222222222222222222","operation":"status","error":false,"reply":{"state":"connected","status":{"state":"connected"}}}`)
	var result ownerResult
	if decodeOwnerJSON(data, &result) != nil {
		t.Fatal("invalid fixture")
	}
	if ownerReplyHasRequiredFields(data, c.Operation, result.Error) {
		t.Fatal("incomplete status accepted")
	}
}

func TestOwnerCancelReachesRunningHandler(t *testing.T) {
	dir := t.TempDir()
	started := make(chan struct{})
	cancelled := make(chan struct{})
	o, err := acquireOwner(dir, func(ctx context.Context, _ OwnerCommand) (OwnerReply, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return OwnerReply{}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := forwardOwner(ctx, dir, OwnerCommand{Operation: "agent-disconnect", AgentID: "hermes"})
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, ErrOwnerOutcomeUnknown) {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not receive cancellation")
	}
}

func TestOwnerNewProcessDoesNotExecuteOldGenerationRequest(t *testing.T) {
	dir := t.TempDir()
	cmd := proc.Command(os.Args[0], "-test.run=^TestOwnerCrossProcessHelper$")
	cmd.Env = append(os.Environ(), "S9_OWNER_HELPER=1", "S9_OWNER_DIR="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "helper-ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper not ready")
		}
		time.Sleep(25 * time.Millisecond)
	}
	data, err := os.ReadFile(filepath.Join(dir, "remote-owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	var old ownerDiscovery
	if err := json.Unmarshal(data, &old); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	id := strings.Repeat("d", 32)
	oldbox := filepath.Join(dir, "remote-owner-"+old.Generation)
	request, _ := json.Marshal(ownerRequest{Generation: old.Generation, ID: id, Expires: time.Now().Add(10 * time.Second), Command: OwnerCommand{Operation: "agent-disconnect", AgentID: "hermes"}})
	if err := os.WriteFile(filepath.Join(oldbox, id+".alive"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldbox, id+".req"), request, 0600); err != nil {
		t.Fatal(err)
	}
	executed := make(chan struct{}, 1)
	o, err := acquireOwner(dir, func(context.Context, OwnerCommand) (OwnerReply, error) {
		executed <- struct{}{}
		return OwnerReply{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	// Replay the previous process's envelope into the live mailbox too: merely
	// leaving it in an abandoned mailbox would not exercise generation checks.
	if err := os.WriteFile(filepath.Join(o.dir, id+".alive"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.dir, id+".req"), request, 0600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		select {
		case <-executed:
			t.Fatal("new owner executed request from prior generation")
		default:
		}
		data, err := os.ReadFile(filepath.Join(o.dir, id+".res"))
		if err == nil {
			var result ownerResult
			if decodeOwnerJSON(data, &result) != nil || result.Error == nil || !*result.Error {
				t.Fatal("old generation was not rejected")
			}
			break
		}
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("replayed envelope was not processed")
		}
		time.Sleep(25 * time.Millisecond)
	}
}
