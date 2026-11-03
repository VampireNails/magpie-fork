package remotejoin

import (
	"context"
	"errors"
)

var ErrOwnerUnavailable = errors.New("remote_owner_unavailable")
var ErrOwnerExists = errors.New("remote_owner_exists")
var ErrOwnerOutcomeUnknown = errors.New("remote_owner_outcome_unknown")

// OwnerCommand is a local operation, never sent to the remote server. Key is
// an agent field name, not a gateway credential. Value is local user input.
type OwnerCommand struct {
	Operation string `json:"operation"`
	ServerURL string `json:"serverUrl,omitempty"`
	AgentID   string `json:"agentId,omitempty"`
	Key       string `json:"key,omitempty"`
	Value     string `json:"value,omitempty"`
}

// OwnerConnection reports the model choice made by ConnectHow. Callers must
// only return a validated field and selected catalog value, not config data.
type OwnerConnection struct {
	How   string `json:"how"`
	Field string `json:"field,omitempty"`
	Value string `json:"value,omitempty"`
}
type OwnerReply struct {
	State         string           `json:"state"`
	Connection    *OwnerConnection `json:"connection,omitempty"`
	Status        *RunnerStatus    `json:"status,omitempty"`
	ManagementURL string           `json:"managementUrl,omitempty"`
}
type OwnerHandler func(context.Context, OwnerCommand) (OwnerReply, error)
