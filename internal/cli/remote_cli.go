package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/yetone/magpie/internal/gateway"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/yetone/magpie/internal/agent"
	"github.com/yetone/magpie/internal/appdir"

	"github.com/yetone/magpie/internal/remotejoin"
)

const remoteUsage = "usage: magpie remote connect [url] | run | status [--json] | refresh | manage [--no-open] | disconnect"

type remoteRequest struct {
	operation string
	serverURL string
	json      bool
	noOpen    bool
}

func parseRemote(args []string) (remoteRequest, error) {
	var req remoteRequest
	if len(args) == 0 {
		return req, errors.New(remoteUsage)
	}
	req.operation = args[0]
	switch req.operation {
	case "connect":
		if len(args) == 2 && len(args[1]) > 0 && args[1][0] != '-' {
			req.serverURL = args[1]
			return req, nil
		}
		if len(args) != 1 {
			return remoteRequest{}, errors.New(remoteUsage)
		}
	case "status":
		if len(args) == 2 && args[1] == "--json" {
			req.json = true
			return req, nil
		}
		if len(args) != 1 {
			return remoteRequest{}, errors.New(remoteUsage)
		}
	case "manage":
		if len(args) == 2 && args[1] == "--no-open" {
			req.noOpen = true
			return req, nil
		}
		if len(args) != 1 {
			return remoteRequest{}, errors.New(remoteUsage)
		}
	case "run", "refresh", "disconnect":
		if len(args) != 1 {
			return remoteRequest{}, errors.New(remoteUsage)
		}
	default:
		return remoteRequest{}, errors.New(remoteUsage)
	}
	return req, nil
}

func remoteCmd(args []string) error {
	req, err := parseRemote(args)
	if err != nil {
		return err
	}
	if req.operation == "connect" && req.serverURL != "" {
		raw := req.serverURL
		if !strings.Contains(raw, "://") {
			raw = "https://" + raw
		}
		root, err := remotejoin.RootOrigin(raw)
		if err != nil {
			return err
		}
		req.serverURL = root
	}
	cmd, ok := remoteOwnerCommand(req)
	if !ok {
		return runRemote()
	}
	reply, err := callRemoteOwner(cmd)
	if err != nil {
		return err
	}
	if req.operation == "manage" {
		if !cleanManagementURL(reply.ManagementURL) {
			return errors.New("management URL unavailable")
		}
		fmt.Println(reply.ManagementURL)
		return nil
	}
	if reply.Status == nil {
		return errors.New("remote status unavailable")
	}
	if req.json {
		return json.NewEncoder(os.Stdout).Encode(reply.Status)
	}
	if req.operation == "status" {
		fmt.Println("state:", reply.Status.State)
		fmt.Println("owner:", reply.Status.Owner, "connected:", reply.Status.Connected)
		if !reply.Status.LastRegistrationAt.IsZero() {
			fmt.Println("registered:", reply.Status.LastRegistrationAt.UTC().Format(time.RFC3339))
		}
		if !reply.Status.LastSyncAt.IsZero() {
			fmt.Println("catalog synchronized:", reply.Status.LastSyncAt.UTC().Format(time.RFC3339))
		}
		if reply.Status.LastErrorCode != "" {
			fmt.Println("error:", reply.Status.LastErrorCode)
		}
		return nil
	}
	fmt.Println(reply.State)
	return nil
}

func callRemoteOwner(cmd remotejoin.OwnerCommand) (remotejoin.OwnerReply, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reply, err := forwardRemote(ctx, cmd)
	if errors.Is(err, remotejoin.ErrOwnerUnavailable) {
		return reply, fmt.Errorf("start magpie remote run: %w", err)
	}
	if errors.Is(err, remotejoin.ErrOwnerOutcomeUnknown) {
		return reply, fmt.Errorf("result unknown; check status before retrying: %w", err)
	}
	return reply, err
}

func cleanManagementURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "/_magpie/remote/v1/browser-login" {
		return false
	}
	_, err = remotejoin.RootOrigin(u.Scheme + "://" + u.Host)
	return err == nil
}

// Managed markers must fail closed: an inaccessible marker is not absence.
func forwardManaged(a *agent.Agent, operation, key, value string) (bool, error) {
	if a.ID != "workbuddy" && a.ID != "hermes" {
		return false, nil
	}
	for _, p := range []string{filepath.Join(appdir.Config(), "remote-connection.json"), filepath.Join(appdir.Config(), "remote-agent-sessions", a.ID+".json")} {
		if _, err := os.Lstat(p); err == nil || !errors.Is(err, os.ErrNotExist) {
			if operation == "agent-pick" && value != "" {
				field := a.Field(key)
				if field == nil || field.Options == nil {
					return true, errors.New("invalid agent choice")
				}
				allowed := false
				for _, option := range field.Options(a.Values()) {
					if option.Value == value {
						allowed = true
						break
					}
				}
				if !allowed {
					return true, errors.New("agent choice is not in the available options")
				}
			}
			if strings.ContainsAny(value, "?#\r\n\t@") || strings.Contains(value, "://") || len(value) > 1024 {
				return true, errors.New("invalid agent choice")
			}
			_, err := callRemoteOwner(remotejoin.OwnerCommand{Operation: operation, AgentID: a.ID, Key: key, Value: value})
			if err == nil {
				fmt.Println("agent operation completed")
			}
			return true, err
		}
	}
	return false, nil
}

// remoteOwnerCommand maps requests to the local owner; run creates the owner
// itself and must never be sent to an existing owner.
func remoteOwnerCommand(req remoteRequest) (remotejoin.OwnerCommand, bool) {
	switch req.operation {
	case "connect":
		return remotejoin.OwnerCommand{Operation: "connect", ServerURL: req.serverURL}, true
	case "status", "refresh", "disconnect", "manage":
		return remotejoin.OwnerCommand{Operation: req.operation}, true
	default:
		return remotejoin.OwnerCommand{}, false
	}
}

var forwardRemote = remotejoin.ForwardToOwner

func remoteAgentChoice(ctx context.Context, id, key, value string) bool {
	if ctx.Err() != nil || (id != "workbuddy" && id != "hermes") {
		return false
	}
	a, err := agent.Find(id)
	if err != nil {
		return false
	}
	field := a.Field(key)
	if field == nil {
		return false
	}
	if value == "" {
		return true
	}
	if len(value) > 1024 || strings.ContainsAny(value, "?#\r\n\t@") || strings.Contains(value, "://") || field.Options == nil {
		return false
	}
	for _, o := range field.Options(a.Values()) {
		if o.Value == value {
			return true
		}
	}
	return false
}

func remoteAgentOperation(ctx context.Context, cmd remotejoin.OwnerCommand) (*remotejoin.OwnerConnection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cmd.AgentID != "workbuddy" && cmd.AgentID != "hermes" {
		return nil, errors.New("invalid agent")
	}
	a, err := agent.Find(cmd.AgentID)
	if err != nil {
		return nil, err
	}
	switch cmd.Operation {
	case "agent-pick":
		return nil, a.Pick(cmd.Key, cmd.Value)
	case "agent-apply":
		return nil, a.Apply(cmd.Key, cmd.Value)
	case "agent-disconnect":
		return nil, a.Disconnect()
	case "agent-default":
		if a.Wired() {
			return nil, a.Disconnect()
		}
		if len(a.Fields) == 0 {
			return nil, errors.New("agent has no fields")
		}
		return nil, a.Pick(a.Fields[0].Key, "")
	case "agent-connect-how":
		c, err := a.ConnectHow()
		if err != nil {
			return nil, err
		}
		return &remotejoin.OwnerConnection{How: c.How, Field: c.Field, Value: c.Value}, nil
	default:
		return nil, errors.New("invalid operation")
	}
}

func runRemote() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	client := remotejoin.NewClient(remotejoin.ClientOptions{})
	runner := remotejoin.NewRunner(client, remotejoin.RunnerOptions{
		Managed: func() (remotejoin.ManagedController, error) {
			store, err := agent.NewManagedStore("")
			if err != nil {
				return nil, err
			}
			return agent.EnableManagedEntries(store)
		},
		AgentOperation: remoteAgentOperation,
		ValidateChoice: remoteAgentChoice,
	})
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	select {
	case err := <-done:
		return finishRemote(runner, err)
	case <-runner.Ready():
	case <-ctx.Done():
		return finishRemote(runner, <-done)
	}
	if ctx.Err() != nil {
		return finishRemote(runner, <-done)
	}
	server := gateway.New()
	gatewayDone := make(chan error, 1)
	go func() { gatewayDone <- server.ListenAndServe(ctx) }()
	select {
	case err := <-done:
		cancel()
		<-gatewayDone
		return finishRemote(runner, err)
	case err := <-gatewayDone:
		cancel()
		closeErr := <-done
		if closeErr != nil {
			return finishRemote(runner, closeErr)
		}
		return err
	case <-ctx.Done():
		err := <-done
		<-gatewayDone
		return finishRemote(runner, err)
	}
}

// Keep the owner process alive whenever the runner cannot release it safely.
func finishRemote(runner *remotejoin.Runner, runErr error) error {
	return finishRemoteWith(runner.Stop, runErr, func() { time.Sleep(5 * time.Second) })
}

func finishRemoteWith(stop func() error, runErr error, wait func()) error {
	if runErr == nil {
		return nil
	}
	if err := stop(); err != nil {
		fmt.Fprintln(os.Stderr, "restore failed; keeping the owner active and retrying")
		for err != nil {
			wait()
			err = stop()
		}
		return nil
	}
	return runErr
}
