package gui

import (
	"context"
	"errors"
	"sync"
)

// desktopRunner reports readiness only after recovery and returns from Run after cleanup.
type desktopRunner interface {
	Run(context.Context) error
	Ready() <-chan struct{}
}
type desktopRemoteLifecycle struct {
	cancel context.CancelFunc
	ready  <-chan struct{}
	done   chan struct{}
	mu     sync.Mutex
	err    error
}

func startDesktopRemote(r desktopRunner) *desktopRemoteLifecycle {
	ctx, cancel := context.WithCancel(context.Background())
	l := &desktopRemoteLifecycle{cancel: cancel, ready: r.Ready(), done: make(chan struct{})}
	go func() { err := r.Run(ctx); l.mu.Lock(); l.err = err; l.mu.Unlock(); close(l.done) }()
	return l
}

// waitReady never starts background work until the runner signals real recovery.
func (l *desktopRemoteLifecycle) waitReady(ctx context.Context) error {
	select {
	case <-l.done:
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.err != nil {
			return l.err
		}
		return errors.New("remote runner stopped before readiness")
	case <-l.ready:
		select {
		case <-l.done:
			return errors.New("remote runner stopped")
		default:
			return nil
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// shutdown returns an error if recovery fails or cannot finish within ctx.
func (l *desktopRemoteLifecycle) shutdown(ctx context.Context) error {
	l.cancel()
	select {
	case <-l.done:
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
