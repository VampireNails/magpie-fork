package remotejoin

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/proc"
)

type runnerManaged struct {
	connected, owner         bool
	recoverFail, restoreFail bool
	recovered, restored      int
}

func (m *runnerManaged) Recover() error {
	m.recovered++
	if m.recoverFail {
		return errors.New("snapshot-secret-canary")
	}
	return nil
}
func (m *runnerManaged) SetConnection(owner, connected bool) { m.owner, m.connected = owner, connected }
func (m *runnerManaged) Restore() error {
	m.connected = false
	m.restored++
	if m.restoreFail {
		return errors.New("snapshot-secret-canary")
	}
	return nil
}
func (m *runnerManaged) Close() error { return m.Restore() }

func runnerFixture(t *testing.T) (*Runner, *runnerManaged, *time.Time, *HealthResult) {
	t.Helper()
	isolateStore(t)
	err := withConnection(func(f *connectionFile) (bool, error) {
		f.SchemaVersion = 1
		f.ServerURL = "https://node.test"
		f.ProviderID = "remote-11111111111111111111111111111111"
		f.ConnectionMode = "auto"
		f.State = "connecting"
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC)
	health := HealthResult{Healthy: true, Reachable: true}
	managed := &runnerManaged{}
	client := NewClient(ClientOptions{Binding: &fakeBinding{}, Now: func() time.Time { return now }})
	runner := NewRunner(client, RunnerOptions{Managed: func() (ManagedController, error) { return managed, nil }, Now: func() time.Time { return now }, Probe: func(context.Context, ClientStatus) HealthResult { return health }})
	// Tests keep registration/catalog off the external network. The real client
	// registration and key path are covered by Client and Health TLS fixtures.
	runner.renew = func(context.Context) (ClientStatus, error) { return client.Status(context.Background()) }
	runner.catalog = func(context.Context) (ClientStatus, error) { return client.Status(context.Background()) }
	if err = runner.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runner.stop() })
	return runner, managed, &now, &health
}
func tickRunner(t *testing.T, r *Runner) RunnerStatus {
	t.Helper()
	r.tick(context.Background())
	s, err := r.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRunnerShortJitterKeepsAdmissionUntilFixedWindow(t *testing.T) {
	r, m, now, health := runnerFixture(t)
	tickRunner(t, r)
	*health = HealthResult{Code: "health_unreachable"}
	tickRunner(t, r)
	*now = now.Add(29 * time.Second)
	s := tickRunner(t, r)
	if !m.connected || m.restored != 0 || s.State != "retrying" {
		t.Fatal("short jitter detached the agent")
	}
	*health = HealthResult{Healthy: true, Reachable: true}
	tickRunner(t, r)
	if !m.connected || m.restored != 0 {
		t.Fatal("jitter recovery restored instead of retaining admission")
	}
}

func TestRunnerOfflineRetriesAreBoundedAndResetOnRecovery(t *testing.T) {
	r, _, _, health := runnerFixture(t)
	r.tick(context.Background())
	*health = HealthResult{Code: "health_unreachable"}
	for _, want := range []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 10 * time.Second} {
		if got := r.tick(context.Background()); got != want {
			t.Fatalf("retry delay = %v, want %v", got, want)
		}
	}
	*health = HealthResult{Healthy: true, Reachable: true}
	r.tick(context.Background())
	*health = HealthResult{Code: "health_unreachable"}
	if got := r.tick(context.Background()); got != 2*time.Second {
		t.Fatalf("retry delay did not reset: %v", got)
	}
}

func TestRunnerRestoresWhenProbeCrossesOfflineWindow(t *testing.T) {
	r, m, now, health := runnerFixture(t)
	tickRunner(t, r)
	*health = HealthResult{Code: "health_unreachable"}
	tickRunner(t, r)
	r.options.Probe = func(context.Context, ClientStatus) HealthResult {
		*now = now.Add(30 * time.Second)
		return HealthResult{Code: "health_unreachable"}
	}
	s := tickRunner(t, r)
	if s.State != "offline-detached" || m.restored != 1 {
		t.Fatal("slow health probe delayed overdue restore")
	}
}

func TestRunnerProbesBeforeSlowRegistration(t *testing.T) {
	r, _, _, _ := runnerFixture(t)
	probed := false
	r.options.Probe = func(context.Context, ClientStatus) HealthResult {
		probed = true
		return HealthResult{Healthy: true, Reachable: true}
	}
	r.renew = func(ctx context.Context) (ClientStatus, error) {
		if !probed {
			t.Error("registration blocked health probe")
		}
		return r.client.Status(ctx)
	}
	tickRunner(t, r)
}

func TestRunnerContinuousOfflineClosesAndRestores(t *testing.T) {
	r, m, now, health := runnerFixture(t)
	tickRunner(t, r)
	*health = HealthResult{Code: "health_unreachable"}
	tickRunner(t, r)
	*now = now.Add(30 * time.Second)
	s := tickRunner(t, r)
	if m.connected || m.restored != 1 || s.State != "offline-detached" {
		t.Fatal("continuous outage did not close admission and restore")
	}
	tickRunner(t, r)
	if m.restored != 1 {
		t.Fatal("completed restore repeated during same outage")
	}
}
func TestRunnerRestoreFailureRetriesBeforeAnyNetwork(t *testing.T) {
	r, m, now, health := runnerFixture(t)
	tickRunner(t, r)
	*health = HealthResult{Code: "health_unreachable"}
	tickRunner(t, r)
	m.restoreFail = true
	*now = now.Add(30 * time.Second)
	s := tickRunner(t, r)
	if s.State != "restore-failed" || m.connected {
		t.Fatal("restore failure counted as restored")
	}
	calls := 0
	r.options.Probe = func(context.Context, ClientStatus) HealthResult {
		calls++
		return HealthResult{Healthy: true, Reachable: true}
	}
	*now = now.Add(5 * time.Second)
	tickRunner(t, r)
	if calls != 0 || m.restored < 2 {
		t.Fatal("network resumed before failed restore retried")
	}
	m.restoreFail = false
	tickRunner(t, r)
	if !m.connected || calls == 0 {
		t.Fatal("restore retry never allowed directory reconnect")
	}
}
func TestRunnerRestartRecoversBeforeRegistrationOrHealth(t *testing.T) {
	r, m, _, _ := runnerFixture(t)
	m.recoverFail = true
	network := 0
	r.renew = func(context.Context) (ClientStatus, error) { network++; return ClientStatus{}, nil }
	r.options.Probe = func(context.Context, ClientStatus) HealthResult { network++; return HealthResult{Healthy: true} }
	s := tickRunner(t, r)
	if network != 0 || s.State != "restore-failed" || m.connected {
		t.Fatal("restart network preceded uncertain journal recovery")
	}
	m.recoverFail = false
	tickRunner(t, r)
	if m.recovered < 2 || network == 0 {
		t.Fatal("startup recovery did not retry")
	}
}
func TestRunnerRecoveredServerNeverCreatesAgentGeneration(t *testing.T) {
	r, m, now, health := runnerFixture(t)
	tickRunner(t, r)
	*health = HealthResult{Code: "health_unreachable"}
	tickRunner(t, r)
	*now = now.Add(30 * time.Second)
	tickRunner(t, r)
	recovered, restored := m.recovered, m.restored
	*health = HealthResult{Healthy: true, Reachable: true}
	s := tickRunner(t, r)
	if !s.Connected || m.recovered != recovered || m.restored != restored {
		t.Fatal("directory reconnect changed agent session")
	}
}
func TestRunnerCatalogOrProviderErrorDoesNotMeanServerOffline(t *testing.T) {
	r, m, now, _ := runnerFixture(t)
	r.catalog = func(context.Context) (ClientStatus, error) {
		return ClientStatus{}, errors.New("provider-error-canary")
	}
	tickRunner(t, r)
	*now = now.Add(time.Hour)
	s := tickRunner(t, r)
	if !m.connected || m.restored != 0 || !s.Connected || s.LastErrorCode != "model_refresh_failed" {
		t.Fatal("catalog/provider failure detached healthy server")
	}
}
func TestRunner401ReregistersBeforeTakingNewHealth(t *testing.T) {
	r, m, _, health := runnerFixture(t)
	tickRunner(t, r)
	repairs := 0
	r.renew = func(context.Context) (ClientStatus, error) {
		repairs++
		*health = HealthResult{Healthy: true, Reachable: true}
		return r.client.Status(context.Background())
	}
	*health = HealthResult{Reachable: true, Unauthorized: true, Code: "health_unauthorized"}
	s := tickRunner(t, r)
	if repairs != 1 || !s.Connected || !m.connected || m.restored != 0 {
		t.Fatal("401 did not repair key through registration")
	}
}
func TestRunnerDisconnectPersistsPausedDespiteRestoreFailure(t *testing.T) {
	r, m, _, _ := runnerFixture(t)
	tickRunner(t, r)
	m.restoreFail = true
	if _, err := r.Disconnect(context.Background()); err == nil {
		t.Fatal("failed restore reported disconnect success")
	}
	saved, err := r.client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if saved.ConnectionMode != "paused" || m.connected {
		t.Fatal("disconnect did not durably pause before restore")
	}
	m.restoreFail = false
	s := tickRunner(t, r)
	if s.State != "paused" || m.connected {
		t.Fatal("restore retry unpaused connection")
	}
}

func TestRunnerReadyOnlyAfterRecoveryAndCloseFailureRetainsOwner(t *testing.T) {
	r, m, _, _ := runnerFixture(t)
	m.recoverFail = true
	tickRunner(t, r)
	select {
	case <-r.Ready():
		t.Fatal("ready before recovery")
	default:
	}
	m.recoverFail = false
	tickRunner(t, r)
	select {
	case <-r.Ready():
	default:
		t.Fatal("recovery did not signal readiness")
	}
	m.restoreFail = true
	if err := r.Stop(); err == nil {
		t.Fatal("failed close reported success")
	}
	other, err := acquireOwner(appdir.Config(), nil)
	if err == nil {
		other.Close()
		t.Fatal("owner released after failed close")
	}
	m.restoreFail = false
	if err = r.Stop(); err != nil {
		t.Fatal("failed close could not retry")
	}
	other, err = acquireOwner(appdir.Config(), nil)
	if err != nil {
		t.Fatal("successful close retained owner")
	}
	other.Close()
}

func TestRunnerSwitchRestoreFailurePublishesBlockedAndRetainsOrigin(t *testing.T) {
	r, m, _, _ := runnerFixture(t)
	t.Cleanup(func() { m.restoreFail = false })
	stamp := time.Date(2026, 11, 3, 1, 2, 3, 0, time.UTC)
	if err := withConnection(func(f *connectionFile) (bool, error) {
		f.LastSyncAt = stamp
		f.DeviceID = "0123456789abcdef01234567"
		f.ManagementURL = f.ServerURL + BrowserLoginPath
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	tickRunner(t, r)
	m.restoreFail = true
	s, err := r.Connect(context.Background(), "https://other.test")
	if err == nil || s.Connected || s.State != "restore-failed" || s.LastErrorCode != "restore_failed" || s.ServerURL != "https://node.test" || !s.LastSyncAt.Equal(stamp) {
		t.Fatal("failed switch did not publish blocked recovery and preserve previous state")
	}
	if _, err = r.ManagementURL(context.Background()); err == nil {
		t.Fatal("management remained available during failed recovery")
	}
	m.restoreFail = false
	s = tickRunner(t, r)
	if s.State == "restore-failed" {
		t.Fatal("restore did not retry")
	}
}

func TestRunnerCanceledQueuedOperationsDoNotWrite(t *testing.T) {
	for _, op := range []string{"connect", "refresh", "disconnect", "agent-default"} {
		t.Run(op, func(t *testing.T) {
			r, m, _, _ := runnerFixture(t)
			tickRunner(t, r)
			calls := 0
			r.options.AgentOperation = func(context.Context, OwnerCommand) (*OwnerConnection, error) { calls++; return nil, nil }
			before := m.restored
			r.op.Lock()
			ctx, cancel := context.WithCancel(context.Background())
			started := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				close(started)
				var err error
				switch op {
				case "connect":
					_, err = r.Connect(ctx, "https://other.test")
				case "refresh":
					_, err = r.Refresh(ctx)
				case "disconnect":
					_, err = r.Disconnect(ctx)
				default:
					_, err = r.handle(ctx, OwnerCommand{Operation: "agent-default", AgentID: "workbuddy"})
				}
				done <- err
			}()
			<-started
			cancel()
			r.op.Unlock()
			select {
			case err := <-done:
				if err == nil {
					t.Error("canceled queued operation succeeded")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("canceled operation did not finish")
			}
			stored, err := r.client.Status(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if m.restored != before || calls != 0 || stored.ConnectionMode != "auto" || stored.ServerURL != "https://node.test" {
				t.Fatal("canceled queued operation changed state")
			}
		})
	}
}

func TestRunnerCanceledWhileWaitingForConnectionStorageDoesNotPause(t *testing.T) {
	r, m, _, _ := runnerFixture(t)
	tickRunner(t, r)
	if err := connectionStorage.LockContext(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() { close(started); _, err := r.Disconnect(ctx); done <- err }()
	<-started
	time.Sleep(50 * time.Millisecond)
	cancel()
	connectionStorage.Unlock()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled storage wait succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled storage wait did not finish")
	}
	s, err := r.client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.ConnectionMode != "auto" || m.restored != 0 || !m.connected {
		t.Fatal("canceled storage wait changed connection")
	}
}

func TestRunnerCanceledOperationStopsWaitingForOperationLock(t *testing.T) {
	r, _, _, _ := runnerFixture(t)
	tickRunner(t, r)
	r.op.Lock()
	defer r.op.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := r.Disconnect(ctx); done <- err }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled wait succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled operation kept waiting for lock")
	}
}

func TestRunnerStopTimeoutRetainsOwnerUntilDirectOperationDrains(t *testing.T) {
	r, m, _, _ := runnerFixture(t)
	tickRunner(t, r)
	r.op.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := r.StopContext(ctx)
	if err == nil {
		r.op.Unlock()
		t.Fatal("stop succeeded while direct operation was active")
	}
	if m.restored != 0 {
		r.op.Unlock()
		t.Fatal("restored before drain")
	}
	if o, e := acquireOwner(appdir.Config(), nil); e == nil {
		o.Close()
		r.op.Unlock()
		t.Fatal("second owner acquired during drain")
	}
	checkOtherProcessOwner(t, false)
	r.op.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = r.Stop()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stop retry did not complete")
		}
	}
	if m.restored != 1 {
		t.Fatal("controller not closed after drain")
	}
	o, err := acquireOwner(appdir.Config(), nil)
	if err != nil {
		t.Fatal("owner not released")
	}
	o.Close()
	checkOtherProcessOwner(t, true)
}

func TestRunnerOwnerAcquireHelper(t *testing.T) {
	if os.Getenv("S9_RUNNER_OWNER_CHECK") != "1" {
		return
	}
	o, err := acquireOwner(os.Getenv("S9_RUNNER_OWNER_DIR"), nil)
	if err != nil {
		os.Exit(17)
	}
	o.Close()
}
func checkOtherProcessOwner(t *testing.T, want bool) {
	t.Helper()
	cmd := proc.Command(os.Args[0], "-test.run=^TestRunnerOwnerAcquireHelper$")
	cmd.Env = append(os.Environ(), "S9_RUNNER_OWNER_CHECK=1", "S9_RUNNER_OWNER_DIR="+appdir.Config())
	err := cmd.Run()
	if (err == nil) != want {
		t.Fatal("separate process owner arbitration mismatch")
	}
}

func TestRunnerStopTimeoutRetainsOwnerWhileHandlerIgnoresCancel(t *testing.T) {
	r, m, _, _ := runnerFixture(t)
	tickRunner(t, r)
	entered, release := make(chan struct{}), make(chan struct{})
	r.options.AgentOperation = func(context.Context, OwnerCommand) (*OwnerConnection, error) {
		close(entered)
		<-release
		return nil, nil
	}
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := forwardOwner(ctx, appdir.Config(), OwnerCommand{Operation: "agent-default", AgentID: "workbuddy"})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err := r.StopContext(ctx)
	cancel()
	if err == nil || m.restored != 0 {
		close(release)
		<-done
		t.Fatal("stop did not retain active handler")
	}
	if o, e := acquireOwner(appdir.Config(), nil); e == nil {
		o.Close()
		close(release)
		<-done
		t.Fatal("owner released before handler ended")
	}
	close(release)
	<-done
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err = r.Stop(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stop retry failed after handler ended")
		}
	}
	if m.restored != 1 {
		t.Fatal("controller not restored after drain")
	}
}

func TestRunnerStopPreventsProbeFromReopeningAdmission(t *testing.T) {
	r, m, _, _ := runnerFixture(t)
	tickRunner(t, r)
	entered, release := make(chan struct{}), make(chan struct{})
	r.options.Probe = func(context.Context, ClientStatus) HealthResult {
		close(entered)
		<-release
		return HealthResult{Healthy: true, Reachable: true}
	}
	done := make(chan struct{})
	go func() { r.tick(context.Background()); close(done) }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err := r.StopContext(ctx)
	cancel()
	if err == nil {
		close(release)
		<-done
		t.Fatal("stop succeeded before probe finished")
	}
	close(release)
	<-done
	if m.connected || r.snapshot().Connected {
		t.Fatal("in-flight probe reopened admission after stop")
	}
	if err = r.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerActiveStopEndsRunAndCannotRestart(t *testing.T) {
	isolateStore(t)
	m := &runnerManaged{}
	c := NewClient(ClientOptions{Binding: &fakeBinding{}})
	r := NewRunner(c, RunnerOptions{Managed: func() (ManagedController, error) { return m, nil }})
	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()
	select {
	case <-r.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not become ready")
	}
	if err := r.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run outlived successful stop")
	}
	if err := r.Run(context.Background()); err == nil {
		t.Fatal("stopped runner restarted")
	}
}

func TestRunnerStopDuringStartWaitsAndClosesOwner(t *testing.T) {
	isolateStore(t)
	m := &runnerManaged{}
	c := NewClient(ClientOptions{Binding: &fakeBinding{}})
	entered, release := make(chan struct{}), make(chan struct{})
	r := NewRunner(c, RunnerOptions{Managed: func() (ManagedController, error) { close(entered); <-release; return m, nil }})
	runDone := make(chan error, 1)
	go func() { runDone <- r.Run(context.Background()) }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err := r.StopContext(ctx)
	cancel()
	if err == nil {
		close(release)
		t.Fatal("stop succeeded during startup")
	}
	close(release)
	<-runDone
	if err = r.Stop(); err != nil {
		t.Fatal(err)
	}
	checkOtherProcessOwner(t, true)
}

type blockingStopController struct {
	runnerManaged
	entered, release chan struct{}
	onStop           func()
}

func (m *blockingStopController) SetConnection(owner, connected bool) {
	if !owner && !connected {
		select {
		case <-m.entered:
		default:
			close(m.entered)
		}
		if m.onStop != nil {
			m.onStop()
		}
		<-m.release
	}
	m.runnerManaged.SetConnection(owner, connected)
}
func TestRunnerStopContextBoundsBlockedController(t *testing.T) {
	isolateStore(t)
	m := &blockingStopController{entered: make(chan struct{}), release: make(chan struct{})}
	c := NewClient(ClientOptions{Binding: &fakeBinding{}})
	r := NewRunner(c, RunnerOptions{Managed: func() (ManagedController, error) { return m, nil }})
	if err := r.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.tick(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := r.StopContext(ctx); err == nil {
		close(m.release)
		t.Fatal("blocked controller reported stop success")
	}
	select {
	case <-m.entered:
	case <-time.After(time.Second):
		close(m.release)
		t.Fatal("controller did not enter")
	}
	checkOtherProcessOwner(t, false)
	close(m.release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := r.Stop(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stop retry failed")
		}
	}
	checkOtherProcessOwner(t, true)
}
func TestRunnerStopContextBoundsReentrantController(t *testing.T) {
	isolateStore(t)
	m := &blockingStopController{entered: make(chan struct{}), release: make(chan struct{})}
	close(m.release)
	r := NewRunner(NewClient(ClientOptions{Binding: &fakeBinding{}}), RunnerOptions{Managed: func() (ManagedController, error) { return m, nil }})
	if err := r.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.tick(context.Background())
	m.onStop = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_ = r.StopContext(ctx)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.StopContext(ctx); err != nil {
		t.Fatal("reentrant callback prevented stop")
	}
}
func TestRunnerStopBeforeStartIsStopped(t *testing.T) {
	r := NewRunner(NewClient(ClientOptions{Binding: &fakeBinding{}}), RunnerOptions{})
	if err := r.Stop(); err != nil {
		t.Fatal(err)
	}
	s, _ := r.Status(context.Background())
	if s.State != "stopped" || s.Owner {
		t.Fatal("unstarted stop status incorrect")
	}
}
