// Package remotejoin provides the trusted bootstrap boundary for remote clients.
// It does not replace the gateway's authentication or the GUI's Web cookies.
package remotejoin

import (
	"context"
	"errors"
	"net/http"
)

type Peer struct {
	StableNodeID string `json:"stableNodeId"`
	NodeID       string `json:"nodeId"`
	DisplayName  string `json:"displayName"`
}

type PeerResolver interface {
	Resolve(context.Context, *http.Request) (Peer, error)
}

var (
	ErrInvalidRequest      = errors.New("invalid request")
	ErrIdentityRequired    = errors.New("Tailscale node identity required")
	ErrIdentityUnavailable = errors.New("Tailscale identity lookup unavailable")
)
