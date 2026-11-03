package remotejoin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/yetone/magpie/internal/filelock"
)

type connectionOwner struct {
	lock       *os.File
	stableLock *stableOwner
	config     *ownerDirectory
	mailbox    *ownerDirectory
	lockInfo   os.FileInfo
	dir        string
	stop       chan struct{}
	done       chan struct{}
	cancel     context.CancelFunc
	stopOnce   sync.Once
	releaseMu  sync.Mutex
	released   bool
}
type ownerDiscovery struct {
	Generation string `json:"generation"`
}
type ownerRequest struct {
	Generation string       `json:"generation"`
	ID         string       `json:"id"`
	Expires    time.Time    `json:"expires"`
	Command    OwnerCommand `json:"command"`
}
type ownerResult struct {
	Generation string      `json:"generation"`
	ID         string      `json:"id"`
	Operation  string      `json:"operation"`
	Reply      *OwnerReply `json:"reply"`
	Error      *bool       `json:"error"`
}

func decodeOwnerJSON(data []byte, out any) error {
	check := json.NewDecoder(bytes.NewReader(data))
	if err := ownerJSONValue(check, 0); err != nil {
		return err
	}
	if _, err := check.Token(); err != io.EOF {
		return errors.New("invalid JSON")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("invalid JSON")
	}
	return nil
}

// ownerJSONValue rejects ambiguous object fields before decoding into structs.
func ownerJSONValue(d *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("JSON nesting limit")
	}
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("invalid JSON key")
			}
			name = strings.Map(func(r rune) rune {
				for {
					next := unicode.SimpleFold(r)
					if next <= r {
						return next
					}
					r = next
				}
			}, name) // encoding/json matches fields using Unicode simple folding.
			if _, exists := seen[name]; exists {
				return errors.New("duplicate JSON key")
			}
			seen[name] = struct{}{}
			if err := ownerJSONValue(d, depth+1); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
	case '[':
		for d.More() {
			if err := ownerJSONValue(d, depth+1); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	return nil
}

func ownerID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func validID(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 16 && s == strings.ToLower(s)
}
func acquireOwner(dir string, handler OwnerHandler) (*connectionOwner, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, ErrOwnerUnavailable
	}
	stable, err := acquireStableOwner(dir)
	if err != nil {
		return nil, err
	}
	keepStable := false
	defer func() {
		if !keepStable {
			releaseStableOwner(stable)
		}
	}()
	if err := verifyOwnerPath(dir); err != nil {
		return nil, ErrOwnerUnavailable
	}
	config, err := openOwnerDirectory(dir)
	if err != nil {
		return nil, ErrOwnerUnavailable
	}
	keep := false
	defer func() {
		if !keep {
			config.root.Close()
		}
	}()
	f, err := config.open("remote-owner.lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, ErrOwnerUnavailable
	}
	ok, err := filelock.TryLock(f)
	if err != nil || !ok {
		f.Close()
		return nil, ErrOwnerExists
	}
	generation, err := ownerID()
	if err != nil {
		filelock.Unlock(f)
		f.Close()
		return nil, ErrOwnerUnavailable
	}
	box := filepath.Join(dir, "remote-owner-"+generation)
	if err = config.root.Mkdir("remote-owner-"+generation, 0700); err != nil {
		filelock.Unlock(f)
		f.Close()
		return nil, ErrOwnerUnavailable
	}
	mailbox, err := openOwnerMailbox(config, "remote-owner-"+generation)
	if err != nil {
		filelock.Unlock(f)
		f.Close()
		return nil, ErrOwnerUnavailable
	}
	data, _ := json.Marshal(ownerDiscovery{generation})
	err = config.write("remote-owner.json", data)
	if err != nil {
		mailbox.root.Close()
		config.root.Remove("remote-owner-" + generation)
		filelock.Unlock(f)
		f.Close()
		return nil, ErrOwnerUnavailable
	}
	serveCtx, cancel := context.WithCancel(context.Background())
	lockInfo, _ := f.Stat()
	o := &connectionOwner{lock: f, stableLock: stable, config: config, mailbox: mailbox, lockInfo: lockInfo, dir: box, stop: make(chan struct{}), done: make(chan struct{}), cancel: cancel}
	keep = true
	keepStable = true
	go o.serve(serveCtx, generation, handler)
	return o, nil
}
func (o *connectionOwner) serve(serveCtx context.Context, generation string, handler OwnerHandler) {
	seen := make(map[string]time.Time)
	defer close(o.done)
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-o.stop:
			return
		case <-tick.C:
		}
		// Expired envelopes can no longer execute, so retaining their IDs for
		// the entire process lifetime would only turn the limit into an outage.
		for id, expires := range seen {
			if !expires.After(time.Now()) {
				delete(seen, id)
			}
		}
		if !o.validIdentity() {
			continue
		}
		directory, err := o.mailbox.root.Open(".")
		if err != nil {
			continue
		}
		entries, err := directory.ReadDir(-1)
		directory.Close()
		if err != nil {
			continue
		}
		for _, entry := range entries {
			ext := filepath.Ext(entry.Name())
			if (ext == ".alive" || ext == ".res" || ext == ".tmp") && validID(strings.TrimSuffix(entry.Name(), ext)) {
				info, e := entry.Info()
				if e == nil && info.Mode().IsRegular() && time.Since(info.ModTime()) > time.Minute {
					_ = o.mailbox.root.Remove(entry.Name())
				}
				continue
			}
			if serveCtx.Err() != nil {
				return
			}
			name := entry.Name()
			if !strings.HasSuffix(name, ".req") || !validID(strings.TrimSuffix(name, ".req")) {
				continue
			}
			info, e := entry.Info()
			if e != nil || !info.Mode().IsRegular() || info.Size() > 8192 {
				o.mailbox.root.Remove(name)
				continue
			}
			data, e := o.mailbox.read(name, 8192)
			o.mailbox.root.Remove(name)
			if e != nil {
				continue
			}
			id := strings.TrimSuffix(name, ".req")
			alive := id + ".alive"
			var request ownerRequest
			var reply OwnerReply
			failed := false
			if decodeOwnerJSON(data, &request) != nil || request.ID != id || request.Generation != generation || request.Expires.IsZero() || !request.Expires.After(time.Now()) || request.Expires.After(time.Now().Add(30*time.Second)) || !seen[id].IsZero() || len(seen) >= 4096 || !o.mailbox.alive(alive) || !validOwnerCommand(request.Command) || handler == nil {
				failed = true
			} else {
				seen[id] = request.Expires
				ctx, cancel := context.WithDeadline(serveCtx, request.Expires)
				watchDone := make(chan struct{})
				go watchOwnerCaller(ctx, cancel, func() bool { return o.mailbox.alive(alive) && o.validIdentity() }, watchDone)
				if ctx.Err() == nil {
					reply, e = handler(ctx, request.Command)
				} else {
					e = ctx.Err()
				}
				cancel()
				<-watchDone
				failed = e != nil
			}
			if serveCtx.Err() != nil || !request.Expires.After(time.Now()) || !o.mailbox.alive(alive) || !o.validIdentity() {
				continue
			}
			if failed {
				reply = OwnerReply{}
			}
			if !failed && !validOwnerReply(request.Command, reply) {
				failed, reply = true, OwnerReply{}
			}
			result := ownerResult{Generation: generation, ID: id, Operation: request.Command.Operation, Reply: &reply, Error: &failed}
			out, _ := json.Marshal(result)
			if len(out) > 8192 {
				continue
			}
			responsePath := id + ".res"
			o.mailbox.write(responsePath, out)
			if !o.mailbox.alive(alive) {
				o.mailbox.root.Remove(responsePath)
			}
		}
	}
}
func (o *connectionOwner) quiesceContext(ctx context.Context) error {
	o.stopOnce.Do(func() { close(o.stop); o.cancel() })
	select {
	case <-o.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// quiesce retains the old runner contract until the caller handles drain errors.
func (o *connectionOwner) quiesce() { _ = o.quiesceContext(context.Background()) }
func (o *connectionOwner) validIdentity() bool {
	info, err := os.Lstat(o.config.root.Name())
	if err != nil || !info.IsDir() || !os.SameFile(info, o.config.info) {
		return false
	}
	info, err = o.config.root.Lstat("remote-owner.lock")
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(info, o.lockInfo) {
		return false
	}
	info, err = o.config.root.Lstat(filepath.Base(o.dir))
	return err == nil && info.IsDir() && os.SameFile(info, o.mailbox.info)
}
func (o *connectionOwner) release() error {
	o.releaseMu.Lock()
	defer o.releaseMu.Unlock()
	if o.released {
		return nil
	}
	select {
	case <-o.done:
	default:
		return errors.New("remote_owner_drain_pending")
	}
	filelock.Unlock(o.lock)
	o.lock.Close()
	o.mailbox.root.Close()
	o.config.root.Close()
	releaseStableOwner(o.stableLock)
	o.released = true
	return nil
}
func (o *connectionOwner) CloseContext(ctx context.Context) error {
	if err := o.quiesceContext(ctx); err != nil {
		return err
	}
	return o.release()
}
func (o *connectionOwner) Close() { _ = o.CloseContext(context.Background()) }
func forwardOwner(ctx context.Context, dir string, command OwnerCommand) (OwnerReply, error) {
	ctx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	if !validOwnerCommand(command) {
		return OwnerReply{}, ErrOwnerUnavailable
	}
	config, err := openOwnerDirectory(dir)
	if err != nil {
		return OwnerReply{}, ErrOwnerUnavailable
	}
	defer config.root.Close()
	f, err := config.open("remote-owner.lock", os.O_RDWR)
	if err != nil {
		return OwnerReply{}, ErrOwnerUnavailable
	}
	held, err := filelock.TryLock(f)
	if held {
		filelock.Unlock(f)
	}
	f.Close()
	if err != nil || held {
		return OwnerReply{}, ErrOwnerUnavailable
	}
	data, err := config.readContext(ctx, "remote-owner.json", 256)
	if err != nil || len(data) > 256 {
		return OwnerReply{}, ErrOwnerUnavailable
	}
	var d ownerDiscovery
	if decodeOwnerJSON(data, &d) != nil || !validID(d.Generation) {
		return OwnerReply{}, ErrOwnerUnavailable
	}
	id, err := ownerID()
	if err != nil {
		return OwnerReply{}, ErrOwnerUnavailable
	}
	mailbox, err := openOwnerMailbox(config, "remote-owner-"+d.Generation)
	if err != nil {
		return OwnerReply{}, ErrOwnerUnavailable
	}
	defer mailbox.root.Close()
	req := id + ".req"
	res := id + ".res"
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if ctx.Err() != nil {
		return OwnerReply{}, ErrOwnerUnavailable
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	data, _ = json.Marshal(ownerRequest{Generation: d.Generation, ID: id, Expires: deadline, Command: command})
	if len(data) > 8192 {
		return OwnerReply{}, ErrOwnerUnavailable
	}
	alive := id + ".alive"
	af, err := mailbox.open(alive, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return OwnerReply{}, ErrOwnerUnavailable
	}
	af.Close()
	defer mailbox.root.Remove(res)
	defer mailbox.root.Remove(req)
	defer mailbox.root.Remove(alive)
	if err = mailbox.write(req, data); err != nil {
		return OwnerReply{}, ErrOwnerUnavailable
	}
	timer := time.NewTicker(25 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return OwnerReply{}, ErrOwnerOutcomeUnknown
		case <-timer.C:
		}
		data, err = mailbox.readContext(ctx, res, 8192)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || len(data) > 8192 {
			return OwnerReply{}, ErrOwnerOutcomeUnknown
		}
		var result ownerResult
		if decodeOwnerJSON(data, &result) != nil || !ownerReplyHasRequiredFields(data, command.Operation, result.Error) || result.Generation != d.Generation || result.ID != id || result.Operation != command.Operation || result.Error == nil || result.Reply == nil {
			return OwnerReply{}, ErrOwnerOutcomeUnknown
		}
		current, e := config.readContext(ctx, "remote-owner.json", 256)
		var latest ownerDiscovery
		if e != nil || decodeOwnerJSON(current, &latest) != nil || latest.Generation != d.Generation {
			return OwnerReply{}, ErrOwnerOutcomeUnknown
		}
		if *result.Error {
			return OwnerReply{}, errors.New("remote_owner_operation_failed")
		}
		if !validOwnerReply(command, *result.Reply) {
			return OwnerReply{}, ErrOwnerOutcomeUnknown
		}
		return *result.Reply, nil
	}
}
func validOwnerCommand(c OwnerCommand) bool {
	if len(c.ServerURL) > 2048 || len(c.Key) > 128 || len(c.Value) > 1024 {
		return false
	}
	switch c.Operation {
	case "status", "refresh", "disconnect", "manage":
		return c.ServerURL == "" && c.AgentID == "" && c.Key == "" && c.Value == ""
	case "connect":
		return c.AgentID == "" && c.Key == "" && c.Value == ""
	case "agent-apply", "agent-pick":
		if c.ServerURL != "" {
			return false
		}
		if c.AgentID == "workbuddy" {
			return c.Key == "provider" && (c.Value == "" || c.Value == "magpie")
		}
		if c.AgentID == "hermes" {
			return c.Key == "model" || c.Key == "effort" || strings.HasPrefix(c.Key, "profile:")
		}
		return false
	case "agent-connect-how", "agent-disconnect", "agent-default":
		return c.ServerURL == "" && (c.AgentID == "hermes" || c.AgentID == "workbuddy") && c.Key == "" && c.Value == ""
	}
	return false
}

func validOwnerReply(command OwnerCommand, reply OwnerReply) bool {
	switch command.Operation {
	case "status", "connect", "refresh", "disconnect":
		if reply.Status == nil || reply.Connection != nil || reply.ManagementURL != "" || reply.State == "" || reply.State != reply.Status.State {
			return false
		}
		switch reply.State {
		case "unconfigured", "connecting", "connected", "retrying", "restoring", "offline-detached", "paused", "restore-failed", "error":
		default:
			return false
		}
		s := reply.Status
		if !s.Owner || (s.State == "connected" && !s.Connected) {
			return false
		}
		if s.ProviderID != "" && (!strings.HasPrefix(s.ProviderID, "remote-") || !validID(strings.TrimPrefix(s.ProviderID, "remote-"))) {
			return false
		}
		if s.DeviceID != "" {
			id, err := hex.DecodeString(s.DeviceID)
			if err != nil || len(id) != 12 || strings.ToLower(s.DeviceID) != s.DeviceID {
				return false
			}
		}
		if s.ConnectionMode != "" && s.ConnectionMode != "auto" && s.ConnectionMode != "paused" {
			return false
		}
		if !validOwnerErrorCode(s.LastErrorCode) {
			return false
		}
		if s.ServerURL != "" {
			origin, e := RootOrigin(s.ServerURL)
			if e != nil || origin != s.ServerURL {
				return false
			}
		}
		if s.ManagementURL != "" && (s.ServerURL == "" || s.ManagementURL != s.ServerURL+BrowserLoginPath) {
			return false
		}
		return true
	case "manage":
		if reply.State != "" || reply.Status != nil || reply.Connection != nil || reply.ManagementURL == "" {
			return false
		}
		valid := false
		err := withConnection(func(f *connectionFile) (bool, error) {
			valid = f.ServerURL != "" && reply.ManagementURL == f.ServerURL+BrowserLoginPath && reply.ManagementURL == f.ManagementURL
			return false, nil
		})
		return err == nil && valid
	case "agent-connect-how":
		if reply.State != "" || reply.Status != nil || reply.ManagementURL != "" || reply.Connection == nil || len(reply.Connection.Field) > 128 || len(reply.Connection.Value) > 1024 {
			return false
		}
		if reply.Connection.Field == "" {
			if reply.Connection.Value != "" {
				return false
			}
		} else if !validOwnerCommand(OwnerCommand{Operation: "agent-pick", AgentID: command.AgentID, Key: reply.Connection.Field, Value: reply.Connection.Value}) {
			return false
		}
		switch reply.Connection.How {
		case "first", "same", "default", "alike", "magpie", "again", "joined", "kept":
			return true
		}
		return false
	case "agent-apply", "agent-pick", "agent-disconnect", "agent-default":
		return reply.State == "" && reply.Status == nil && reply.ManagementURL == "" && reply.Connection == nil
	}
	return false
}

func watchOwnerCaller(ctx context.Context, cancel context.CancelFunc, alive func() bool, done chan<- struct{}) {
	defer close(done)
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if !alive() {
				cancel()
				return
			}
		}
	}
}
func ownerReplyHasRequiredFields(data []byte, operation string, failed *bool) bool {
	if failed == nil {
		return false
	}
	if *failed {
		var result ownerResult
		return decodeOwnerJSON(data, &result) == nil && result.Reply != nil && *result.Reply == (OwnerReply{})
	}
	var outer map[string]json.RawMessage
	if json.Unmarshal(data, &outer) != nil {
		return false
	}
	var reply map[string]json.RawMessage
	if json.Unmarshal(outer["reply"], &reply) != nil {
		return false
	}
	if v, ok := reply["state"]; !ok || bytes.Equal(v, []byte("null")) {
		return false
	}
	switch operation {
	case "status", "connect", "refresh", "disconnect":
		var status map[string]json.RawMessage
		if json.Unmarshal(reply["status"], &status) != nil {
			return false
		}
		for _, key := range []string{"state", "serverUrl", "providerId", "connectionMode", "owner", "connected"} {
			v, ok := status[key]
			if !ok || bytes.Equal(v, []byte("null")) {
				return false
			}
		}
	case "agent-connect-how":
		var connection map[string]json.RawMessage
		if json.Unmarshal(reply["connection"], &connection) != nil {
			return false
		}
		if v, ok := connection["how"]; !ok || bytes.Equal(v, []byte("null")) {
			return false
		}
	}
	return true
}

func validOwnerErrorCode(code string) bool {
	switch code {
	case "", "registration_request_invalid", "registration_unreachable", "registration_rejected", "registration_response_invalid", "provider_binding_failed", "model_refresh_failed", "tailscale_tool_unavailable", "tailscale_status_unavailable", "tailscale_mapping_unavailable", "tls_certificate_invalid", "dns_resolution_failed", "registration_timeout", "registration_failed", "connection_storage_unavailable", "restore_failed", "health_request_invalid", "health_binding_unavailable", "health_signing_failed", "health_unreachable", "health_unauthorized", "health_gateway_failed", "health_response_invalid":
		return true
	}
	return false
}
