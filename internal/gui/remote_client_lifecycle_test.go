package gui

import (
	"context"
	"errors"
	"testing"
	"time"
)

type lifecycleFixture struct {
	ready  chan struct{}
	finish chan error
}

func (f *lifecycleFixture) Ready() <-chan struct{} { return f.ready }
func (f *lifecycleFixture) Run(ctx context.Context) error {
	select {
	case err := <-f.finish:
		return err
	case <-ctx.Done():
		return <-f.finish
	}
}
func TestRemoteClientLifecycleWaitsForRecovery(t *testing.T) {
	f := &lifecycleFixture{make(chan struct{}), make(chan error, 1)}
	l := startDesktopRemote(f)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := l.waitReady(ctx, f); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("premature ready: %v", err)
	}
	close(f.ready)
	if err := l.waitReady(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	f.finish <- nil
	if err := l.shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestRemoteClientLifecycleReportsShutdownFailure(t *testing.T) {
	f := &lifecycleFixture{make(chan struct{}), make(chan error, 1)}
	l := startDesktopRemote(f)
	f.finish <- errors.New("restore failed")
	if err := l.shutdown(context.Background()); err == nil {
		t.Fatal("lost restore failure")
	}
}
func TestRemoteClientLifecycleShutdownIsBounded(t *testing.T) {
	f := &lifecycleFixture{make(chan struct{}), make(chan error, 1)}
	l := startDesktopRemote(f)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := l.shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown: %v", err)
	}
	f.finish <- nil
	if err := l.shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
