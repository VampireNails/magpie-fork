package gui

import (
	"context"

	"github.com/yetone/magpie/internal/agent"
	"github.com/yetone/magpie/internal/remotejoin"
)

type desktopRunnerService struct{ runner *remotejoin.Runner }

func newDesktopRunner() *remotejoin.Runner {
	client := remotejoin.NewClient(remotejoin.ClientOptions{})
	return remotejoin.NewRunner(client, remotejoin.RunnerOptions{Managed: func() (remotejoin.ManagedController, error) {
		store, err := agent.NewManagedStore("")
		if err != nil {
			return nil, err
		}
		return agent.EnableManagedEntries(store)
	}})
}
func projectRemote(s remotejoin.RunnerStatus) remoteClientStatus {
	owner, connected := s.Owner, s.Connected
	out := remoteClientStatus{ServerURL: s.ServerURL, State: s.State, Owner: &owner, Connected: &connected, ErrorCode: s.LastErrorCode}
	if !s.LastSyncAt.IsZero() {
		t := s.LastSyncAt
		out.LastSyncAt = &t
	}
	if s.State == "restore-failed" {
		out.ErrorCode = "RESTORE_FAILED"
	}
	return out
}
func (d desktopRunnerService) Status(ctx context.Context) (remoteClientStatus, error) {
	s, e := d.runner.Status(ctx)
	return projectRemote(s), e
}
func (d desktopRunnerService) Connect(ctx context.Context, raw string) (remoteClientStatus, error) {
	s, e := d.runner.Connect(ctx, raw)
	return projectRemote(s), e
}
func (d desktopRunnerService) Refresh(ctx context.Context) (remoteClientStatus, error) {
	s, e := d.runner.Refresh(ctx)
	return projectRemote(s), e
}
func (d desktopRunnerService) Disconnect(ctx context.Context) (remoteClientStatus, error) {
	s, e := d.runner.Disconnect(ctx)
	return projectRemote(s), e
}
func (d desktopRunnerService) ManagementURL(ctx context.Context) (string, error) {
	return d.runner.ManagementURL(ctx)
}

// stopDesktopRunner never invokes Stop from a managed controller callback.
func stopDesktopRunner(ctx context.Context, life *desktopRemoteLifecycle, r *remotejoin.Runner) error {
	err := r.StopContext(ctx)
	if err != nil {
		return err
	}
	select {
	case <-life.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
