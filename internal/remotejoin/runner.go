package remotejoin

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yetone/magpie/internal/appdir"
)

type ManagedController interface {
	Recover() error
	SetConnection(bool, bool)
	Restore() error
	Close() error
}
type RunnerOptions struct {
	Managed        func() (ManagedController, error)
	Probe          func(context.Context, ClientStatus) HealthResult
	Now            func() time.Time
	Wait           func(context.Context, time.Duration) error
	Policy         RunnerPolicy
	AgentOperation func(context.Context, OwnerCommand) (*OwnerConnection, error)
	// ValidateChoice verifies a field value against the live agent's options.
	ValidateChoice func(context.Context, string, string, string) bool
}
type RunnerPolicy struct {
	ProbeEvery, ProbeTimeout, OfflineWindow, RestoreRetry, RegistrationEvery, CatalogEvery time.Duration
	Retry                                                                                  []time.Duration
}
type RunnerStatus struct {
	ClientStatus
	Owner        bool      `json:"owner"`
	Connected    bool      `json:"connected"`
	LastHealthAt time.Time `json:"lastHealthAt,omitempty"`
}
type Runner struct {
	client            *Client
	options           RunnerOptions
	renew, catalog    func(context.Context) (ClientStatus, error)
	op                sync.Mutex
	refreshMu         sync.Mutex
	refreshCancel     context.CancelFunc
	refreshGeneration uint64
	mu                sync.RWMutex
	state             RunnerStatus
	managed           ManagedController
	owner             *connectionOwner
	ready             chan struct{}
	readyOnce         sync.Once
	recoverPending    bool
	restorePending    bool
	offlineRestored   bool
	offlineSince      time.Time
	nextRegistration  time.Time
	nextCatalog       time.Time
	quiesced          bool
	retryCount        int
	stopMu            sync.Mutex
	stopAttemptState  *runnerStopAttempt
	stopping          atomic.Bool
	admissionMu       sync.Mutex
	lifecycle         sync.Mutex
	runCancel         context.CancelFunc
	runStarted        bool
}
type runnerStopAttempt struct {
	done chan struct{}
	err  error
}

// NewRunner constructs a stopped runner. Managed must install the controller
// with admission closed; it is invoked only after the owner lock is held.
func NewRunner(c *Client, o RunnerOptions) *Runner {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Probe == nil {
		o.Probe = c.probeHealth
	}
	if o.Policy.ProbeEvery <= 0 {
		o.Policy.ProbeEvery = 10 * time.Second
	}
	if o.Policy.ProbeTimeout <= 0 {
		o.Policy.ProbeTimeout = 3 * time.Second
	}
	if len(o.Policy.Retry) == 0 {
		o.Policy.Retry = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second}
	}
	for _, delay := range o.Policy.Retry {
		if delay <= 0 {
			o.Policy.Retry = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second}
			break
		}
	}
	if o.Policy.OfflineWindow <= 0 {
		o.Policy.OfflineWindow = 30 * time.Second
	}
	if o.Policy.RestoreRetry <= 0 {
		o.Policy.RestoreRetry = 5 * time.Second
	}
	if o.Policy.RegistrationEvery <= 0 {
		o.Policy.RegistrationEvery = time.Minute
	}
	if o.Policy.CatalogEvery <= 0 {
		o.Policy.CatalogEvery = 10 * time.Minute
	}
	r := &Runner{client: c, options: o, ready: make(chan struct{}), recoverPending: true}
	r.renew = c.registerOnly
	r.catalog = c.refreshCatalog
	return r
}
func (r *Runner) publish(s RunnerStatus) {
	if origin, err := RootOrigin(s.ServerURL); err != nil || origin != s.ServerURL {
		s.ServerURL = ""
	}
	s.ManagementURL = ""
	r.mu.Lock()
	if r.stopping.Load() {
		s.Connected = false
		if s.State != "restore-failed" && s.Owner {
			s.State = "restoring"
		}
	}
	r.state = s
	r.mu.Unlock()
}
func (r *Runner) snapshot() RunnerStatus { r.mu.RLock(); defer r.mu.RUnlock(); return r.state }

// lockOperation lets expired commands leave the queue without executing.
func (r *Runner) lockOperation(ctx context.Context) error {
	return r.lockOperationMode(ctx, false, false)
}
func (r *Runner) lockOperationMode(ctx context.Context, stopping, preemptRefresh bool) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	var nextProbe time.Time
	for {
		if !stopping && r.stopping.Load() {
			return errors.New("remote_owner_stopping")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if r.op.TryLock() {
			if !stopping && r.stopping.Load() {
				r.op.Unlock()
				return errors.New("remote_owner_stopping")
			}
			if err := ctx.Err(); err != nil {
				r.op.Unlock()
				return err
			}
			return nil
		}
		if preemptRefresh && !time.Now().Before(nextProbe) {
			r.refreshMu.Lock()
			active := r.refreshCancel != nil
			generation := r.refreshGeneration
			r.refreshMu.Unlock()
			if active {
				// Health can be checked independently while registration waits on
				// the network. Only interrupt the same refresh if health fails.
				callCtx, cancel := context.WithTimeout(ctx, r.options.Policy.ProbeTimeout)
				health := r.options.Probe(callCtx, r.snapshot().ClientStatus)
				cancel()
				nextProbe = time.Now().Add(r.options.Policy.ProbeEvery)
				if !health.Healthy && ctx.Err() == nil {
					r.refreshMu.Lock()
					if r.refreshCancel != nil && r.refreshGeneration == generation {
						r.refreshCancel()
					}
					r.refreshMu.Unlock()
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func (r *Runner) start(ctx context.Context) error {
	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	if r.stopping.Load() {
		return errors.New("remote_owner_stopping")
	}
	if r.options.Managed == nil {
		return errors.New("managed_controller_required")
	}
	o, err := acquireOwner(appdir.Config(), r.handle)
	if err != nil {
		return err
	}
	r.owner = o
	m, err := r.options.Managed()
	if err != nil || m == nil {
		o.Close()
		r.owner = nil
		return errors.New("managed_controller_unavailable")
	}
	r.admissionMu.Lock()
	r.managed = m
	m.SetConnection(true, false)
	r.admissionMu.Unlock()
	s, err := r.client.Status(ctx)
	if err != nil {
		r.publish(RunnerStatus{ClientStatus: ClientStatus{State: "error", LastErrorCode: "connection_storage_unavailable"}, Owner: true})
		return nil
	}
	r.publish(RunnerStatus{ClientStatus: s, Owner: true})
	return nil
}
func (r *Runner) stopAttempt(ctx context.Context) error {
	for !r.lifecycle.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	r.lifecycle.Unlock()
	if r.owner == nil {
		s := r.snapshot()
		s.Owner = false
		s.Connected = false
		s.State = "stopped"
		s.LastErrorCode = ""
		r.publish(s)
		return nil
	}
	r.admissionMu.Lock()
	r.managed.SetConnection(false, false)
	r.admissionMu.Unlock()
	if !r.quiesced {
		if err := r.owner.quiesceContext(ctx); err != nil {
			return err
		}
		r.quiesced = true
	}
	if err := r.lockOperationMode(ctx, true, false); err != nil {
		return err
	}
	defer r.op.Unlock()
	r.managed.SetConnection(false, false)
	err := r.managed.Close()
	s := r.snapshot()
	s.Connected = false
	if err != nil {
		s.State = "restore-failed"
		s.LastErrorCode = "restore_failed"
		r.publish(s)
		return errors.New("restore_failed")
	}
	if err := r.owner.release(); err != nil {
		return errors.New("owner_release_failed")
	}
	s.Owner = false
	s.State = "stopped"
	s.LastErrorCode = ""
	r.publish(s)
	r.owner = nil
	return nil
}

// Stop retries recovery and controller removal after Run reports a failure.
// The owner lock remains held until the controller closes successfully.
func (r *Runner) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return r.StopContext(ctx)
}

// StopContext blocks admission and waits for actual drain and recovery. A
// timed-out attempt retains ownership until its work has really finished.
func (r *Runner) StopContext(ctx context.Context) error {
	retried := false
	for {
		r.stopMu.Lock()
		if attempt := r.stopAttemptState; attempt != nil {
			select {
			case <-attempt.done:
				if attempt.err == nil {
					r.stopMu.Unlock()
					return nil
				}
				err := attempt.err
				r.stopAttemptState = nil
				r.stopMu.Unlock()
				// An attempt that failed on the previous caller's context must
				// not hand that error to a caller that still has budget.
				if retried {
					return err
				}
				retried = true
				continue
			default:
				r.stopMu.Unlock()
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-attempt.done:
				}
				continue
			}
		}
		r.stopping.Store(true)
		if r.runCancel != nil {
			r.runCancel()
		}
		s := r.snapshot()
		s.Connected = false
		s.State = "restoring"
		r.publish(s)
		attempt := &runnerStopAttempt{done: make(chan struct{})}
		r.stopAttemptState = attempt
		go func() { attempt.err = r.stopAttempt(ctx); close(attempt.done) }()
		r.stopMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-attempt.done:
		}
		return attempt.err
	}
}

func (r *Runner) stop() error { return r.Stop() }

// Ready closes only after the real owner has recovered old journals.
func (r *Runner) Ready() <-chan struct{} { return r.ready }

// Run owns the lease until cancellation, then restores before releasing it.
func (r *Runner) Run(ctx context.Context) error {
	r.stopMu.Lock()
	if r.stopping.Load() || r.runStarted {
		r.stopMu.Unlock()
		return errors.New("remote_runner_unavailable")
	}
	r.runStarted = true
	ctx, cancel := context.WithCancel(ctx)
	r.runCancel = cancel
	r.stopMu.Unlock()
	defer cancel()
	if err := r.start(ctx); err != nil {
		return err
	}
	for ctx.Err() == nil {
		delay := r.tick(ctx)
		if r.options.Wait != nil {
			if r.options.Wait(ctx, delay) != nil {
				break
			}
		} else {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
			case <-timer.C:
			}
		}
	}
	return r.stop()
}

// detachLostState closes admission if the active connection's local state is
// lost. A failed restore remains pending and is retried before any network.
func (r *Runner) detachLostState(s *RunnerStatus) error {
	if !s.Connected {
		return nil
	}
	r.managed.SetConnection(true, false)
	s.Connected = false
	if err := r.managed.Restore(); err != nil {
		r.restorePending = true
		s.State = "restore-failed"
		s.LastErrorCode = "restore_failed"
		return err
	}
	r.offlineRestored = true
	return nil
}

func (r *Runner) tick(ctx context.Context) time.Duration {
	if err := r.lockOperationMode(ctx, false, true); err != nil {
		return r.options.Policy.ProbeEvery
	}
	defer r.op.Unlock()
	if ctx.Err() != nil {
		return r.options.Policy.ProbeEvery
	}
	s := r.snapshot()
	now := r.options.Now()
	if r.recoverPending {
		if err := r.managed.Recover(); err != nil {
			s.State = "restore-failed"
			s.LastErrorCode = "restore_failed"
			r.publish(s)
			return r.options.Policy.RestoreRetry
		}
		r.recoverPending = false
		r.readyOnce.Do(func() { close(r.ready) })
	}
	if r.restorePending {
		if err := r.managed.Restore(); err != nil {
			s.State = "restore-failed"
			s.LastErrorCode = "restore_failed"
			r.publish(s)
			return r.options.Policy.RestoreRetry
		}
		r.restorePending = false
		r.offlineRestored = true
	}
	// Include time spent waiting for storage in the same outage window.
	// LockContext honors this deadline, so a contended lock cannot delay
	// restoration by another full storage-lock timeout on each tick.
	readStart := r.options.Now()
	readBudget := r.options.Policy.OfflineWindow
	if s.Connected && !r.offlineSince.IsZero() {
		readBudget -= readStart.Sub(r.offlineSince)
	}
	if readBudget <= 0 {
		// Give already-recovered storage a brief chance to report healthy
		// before restoring, without waiting for the full lock timeout.
		readBudget = 100 * time.Millisecond
	}
	readCtx, cancelRead := context.WithTimeout(ctx, readBudget)
	stored, err := r.client.Status(readCtx)
	cancelRead()
	if ctx.Err() != nil {
		return r.options.Policy.ProbeEvery
	}
	if err != nil {
		now := r.options.Now()
		if r.offlineSince.IsZero() {
			r.offlineSince = readStart
		}
		if remaining := r.options.Policy.OfflineWindow - now.Sub(r.offlineSince); s.Connected && remaining > 0 {
			s.State = "retrying"
			s.LastErrorCode = "connection_storage_unavailable"
			r.publish(s)
			return min(r.options.Policy.ProbeEvery, remaining)
		}
		if r.detachLostState(&s) != nil {
			r.publish(s)
			return r.options.Policy.RestoreRetry
		}
		s.State = "error"
		s.LastErrorCode = "connection_storage_unavailable"
		r.publish(s)
		return r.options.Policy.RestoreRetry
	}
	s.ClientStatus = stored
	if stored.ConnectionMode == "paused" {
		s.State = "paused"
		s.Connected = false
		r.publish(s)
		return r.options.Policy.ProbeEvery
	}
	if stored.ServerURL == "" {
		// The connection file can disappear while this owner is running. Do
		// not leave its previous agent wiring admitted with no server to use.
		if r.detachLostState(&s) != nil {
			r.publish(s)
			return r.options.Policy.RestoreRetry
		}
		s.State = "unconfigured"
		r.publish(s)
		return r.options.Policy.ProbeEvery
	}
	probe := func() HealthResult {
		callCtx, cancel := context.WithTimeout(ctx, r.options.Policy.ProbeTimeout)
		defer cancel()
		return r.options.Probe(callCtx, stored)
	}
	h := probe()
	if h.Unauthorized || h.Code == "health_binding_unavailable" {
		callCtx, cancel := context.WithTimeout(ctx, r.options.Policy.ProbeTimeout)
		_, err = r.renew(callCtx)
		cancel()
		r.nextRegistration = r.options.Now().Add(r.options.Policy.RegistrationEvery)
		if err == nil {
			h = probe()
		}
	}
	now = r.options.Now()
	if h.Healthy {
		r.admissionMu.Lock()
		if r.stopping.Load() {
			r.admissionMu.Unlock()
			return r.options.Policy.ProbeEvery
		}
		r.retryCount = 0
		r.offlineSince = time.Time{}
		r.offlineRestored = false
		r.managed.SetConnection(true, true)
		r.admissionMu.Unlock()
		s.Connected = true
		s.State = "connected"
		s.LastHealthAt = now
		s.LastErrorCode = ""
		if !now.Before(r.nextRegistration) {
			callCtx, cancel := context.WithTimeout(ctx, r.options.Policy.ProbeTimeout)
			_, _ = r.renew(callCtx)
			cancel()
			r.nextRegistration = r.options.Now().Add(r.options.Policy.RegistrationEvery)
		}
		if !now.Before(r.nextCatalog) {
			callCtx, cancel := context.WithTimeout(ctx, r.options.Policy.ProbeTimeout)
			_, err = r.catalog(callCtx)
			cancel()
			if err != nil {
				s.LastErrorCode = "model_refresh_failed"
			} else if fresh, e := r.client.Status(ctx); e == nil {
				s.LastSyncAt = fresh.LastSyncAt
			}
			r.nextCatalog = now.Add(r.options.Policy.CatalogEvery)
		}
	} else {
		if r.offlineSince.IsZero() {
			// A failed health probe follows storage acquisition; include any
			// wait there in this continuous unavailable interval.
			r.offlineSince = readStart
		}
		s.LastErrorCode = h.Code
		if now.Sub(r.offlineSince) >= r.options.Policy.OfflineWindow {
			r.managed.SetConnection(true, false)
			s.Connected = false
			if !r.offlineRestored {
				if err = r.managed.Restore(); err != nil {
					r.restorePending = true
					s.State = "restore-failed"
					s.LastErrorCode = "restore_failed"
					r.publish(s)
					return r.options.Policy.RestoreRetry
				}
				r.offlineRestored = true
			}
			s.State = "offline-detached"
		} else {
			s.State = "retrying"
		}
		if !r.offlineRestored {
			index := r.retryCount
			if index >= len(r.options.Policy.Retry) {
				index = len(r.options.Policy.Retry) - 1
			}
			delay := r.options.Policy.Retry[index]
			if remaining := r.options.Policy.OfflineWindow - now.Sub(r.offlineSince); remaining > 0 && delay > remaining {
				delay = remaining
			}
			if r.retryCount < len(r.options.Policy.Retry)-1 {
				r.retryCount++
			}
			r.publish(s)
			return delay
		}
	}
	r.publish(s)
	return r.options.Policy.ProbeEvery
}
func (r *Runner) Status(ctx context.Context) (RunnerStatus, error) {
	if err := ctx.Err(); err != nil {
		return RunnerStatus{}, err
	}
	return r.snapshot(), nil
}
func (r *Runner) Disconnect(ctx context.Context) (RunnerStatus, error) {
	if err := r.lockOperation(ctx); err != nil {
		return r.snapshot(), err
	}
	defer r.op.Unlock()
	if err := ctx.Err(); err != nil {
		return r.snapshot(), err
	}
	if err := withConnectionContext(ctx, func(f *connectionFile) (bool, error) { f.ConnectionMode = "paused"; return true, nil }); err != nil {
		return r.snapshot(), errors.New("connection_storage_unavailable")
	}
	r.managed.SetConnection(true, false)
	s := r.snapshot()
	s.ConnectionMode = "paused"
	s.Connected = false
	if err := r.managed.Restore(); err != nil {
		r.restorePending = true
		s.State = "restore-failed"
		s.LastErrorCode = "restore_failed"
		r.publish(s)
		return s, errors.New("restore_failed")
	}
	s.State = "paused"
	r.publish(s)
	return s, nil
}
func (r *Runner) handle(ctx context.Context, c OwnerCommand) (OwnerReply, error) {
	if !r.snapshot().Owner {
		return OwnerReply{}, errors.New("remote_owner_unavailable")
	}
	switch c.Operation {
	case "status":
		s, e := r.Status(ctx)
		return OwnerReply{State: s.State, Status: &s}, e
	case "disconnect":
		s, e := r.Disconnect(ctx)
		return OwnerReply{State: s.State, Status: &s}, e
	case "connect":
		s, e := r.Connect(ctx, c.ServerURL)
		return OwnerReply{State: s.State, Status: &s}, e
	case "refresh":
		s, e := r.Refresh(ctx)
		return OwnerReply{State: s.State, Status: &s}, e
	case "manage":
		url, e := r.ManagementURL(ctx)
		return OwnerReply{ManagementURL: url}, e
	}
	if r.options.AgentOperation == nil {
		return OwnerReply{}, errors.New("operation_unavailable")
	}
	if err := r.lockOperation(ctx); err != nil {
		return OwnerReply{}, err
	}
	defer r.op.Unlock()
	if err := ctx.Err(); err != nil {
		return OwnerReply{}, err
	}
	if !r.snapshot().Connected {
		return OwnerReply{}, errors.New("remote_not_connected")
	}
	if c.Operation == "agent-apply" || c.Operation == "agent-pick" {
		if r.options.ValidateChoice == nil || !r.options.ValidateChoice(ctx, c.AgentID, c.Key, c.Value) {
			return OwnerReply{}, errors.New("invalid_agent_choice")
		}
	}
	connection, err := r.options.AgentOperation(ctx, c)
	if err == nil && connection != nil && connection.Field != "" {
		if r.options.ValidateChoice == nil || !r.options.ValidateChoice(ctx, c.AgentID, connection.Field, connection.Value) {
			return OwnerReply{}, errors.New("invalid_agent_choice")
		}
	}
	return OwnerReply{Connection: connection}, err
}

func (r *Runner) Connect(ctx context.Context, raw string) (RunnerStatus, error) {
	if err := r.lockOperation(ctx); err != nil {
		return r.snapshot(), err
	}
	defer r.op.Unlock()
	if err := ctx.Err(); err != nil {
		return r.snapshot(), err
	}
	if r.recoverPending || r.restorePending {
		return r.snapshot(), errors.New("restore_pending")
	}
	if raw == "" {
		s, err := r.client.Status(ctx)
		if err != nil {
			return r.snapshot(), err
		}
		raw = s.ServerURL
	}
	if raw == "" {
		return r.snapshot(), errors.New("server_not_configured")
	}
	previous := r.client.beforeSwitch
	r.client.beforeSwitch = func(context.Context) error {
		r.managed.SetConnection(true, false)
		state := r.snapshot()
		state.Connected = false
		state.State = "restoring"
		r.publish(state)
		if err := r.managed.Restore(); err != nil {
			r.restorePending = true
			state.State = "restore-failed"
			state.LastErrorCode = "restore_failed"
			r.publish(state)
			return errors.New("restore_failed")
		}
		if previous != nil {
			return previous(ctx)
		}
		return nil
	}
	defer func() { r.client.beforeSwitch = previous }()
	prior := r.snapshot()
	configured, err := r.client.Configure(ctx, raw)
	s := r.snapshot()
	if err != nil {
		if r.restorePending {
			return s, errors.New("restore_failed")
		}
		s.State = "connecting"
		s.LastErrorCode = "registration_failed"
		r.publish(s)
		return s, errors.New("connection_failed")
	}
	s.ClientStatus = configured
	if configured.ServerURL != prior.ServerURL {
		s.Connected = false
		s.LastHealthAt = time.Time{}
	}
	if s.Connected {
		s.State = "connected"
	} else {
		s.State = "connecting"
	}
	r.publish(s)
	r.nextRegistration = time.Time{}
	return r.snapshot(), nil
}
func (r *Runner) Refresh(ctx context.Context) (RunnerStatus, error) {
	if err := r.lockOperation(ctx); err != nil {
		return r.snapshot(), err
	}
	defer r.op.Unlock()
	if err := ctx.Err(); err != nil {
		return r.snapshot(), err
	}
	if r.recoverPending || r.restorePending {
		return r.snapshot(), errors.New("restore_pending")
	}
	refreshCtx, cancel := context.WithCancel(ctx)
	r.refreshMu.Lock()
	r.refreshGeneration++
	r.refreshCancel = cancel
	r.refreshMu.Unlock()
	defer func() {
		r.refreshMu.Lock()
		r.refreshCancel = nil
		r.refreshMu.Unlock()
		cancel()
	}()
	refreshed, err := r.client.Refresh(refreshCtx)
	if err != nil {
		if refreshCtx.Err() != nil {
			return r.snapshot(), refreshCtx.Err()
		}
		s := r.snapshot()
		if refreshed.ServerURL == s.ServerURL && refreshed.ProviderID == s.ProviderID {
			s.LastErrorCode = refreshed.LastErrorCode
		}
		if s.LastErrorCode == "" {
			s.LastErrorCode = "registration_failed"
		}
		r.publish(s)
		return r.snapshot(), errors.New(s.LastErrorCode)
	}
	s := r.snapshot()
	s.ClientStatus = refreshed
	if s.Connected {
		s.State = "connected"
	} else {
		s.State = "connecting"
	}
	r.publish(s)
	return r.snapshot(), nil
}
func (r *Runner) ManagementURL(ctx context.Context) (string, error) {
	s := r.snapshot()
	if !s.Owner || !s.Connected {
		return "", errors.New("management_unavailable")
	}
	stored, err := r.client.Status(ctx)
	if err != nil || stored.ServerURL == "" || stored.ManagementURL != stored.ServerURL+BrowserLoginPath {
		return "", errors.New("management_unavailable")
	}
	return stored.ManagementURL, nil
}

// ForwardToOwner submits one local operation to the current owner. An unknown
// outcome must not be replayed automatically.
func ForwardToOwner(ctx context.Context, c OwnerCommand) (OwnerReply, error) {
	return forwardOwner(ctx, appdir.Config(), c)
}
