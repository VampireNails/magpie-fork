// Package remotejoin provides the trusted bootstrap boundary for remote clients.
// It does not replace the gateway's authentication or the GUI's Web cookies.
package remotejoin

import (
	"context"
	"errors"
	"net/http"

	"github.com/yetone/magpie/internal/access"
)

type Peer struct {
	StableNodeID string `json:"stableNodeId"`
	NodeID       string `json:"nodeId"`
	DisplayName  string `json:"displayName"`
}

type PeerResolver interface {
	Resolve(context.Context, *http.Request) (Peer, error)
}

type DeviceCredential = access.DeviceCredential
type DevicePage = access.DevicePage

type DeviceStore interface {
	EnsureDevice(context.Context, Peer) (DeviceCredential, error)
	ListDevices(context.Context, string, int) (DevicePage, error)
	RemoveDevice(context.Context, string) error
}

// AccessDeviceStore adapts the native credential store without making access
// depend on this transport package. Only EnsureDevice may return a secret.
type AccessDeviceStore struct{}

func (AccessDeviceStore) EnsureDevice(ctx context.Context, p Peer) (DeviceCredential, error) {
	return access.EnsureDevice(ctx, access.DeviceIdentity{StableNodeID: p.StableNodeID, NodeID: p.NodeID, DisplayName: p.DisplayName})
}
func (AccessDeviceStore) ListDevices(ctx context.Context, cursor string, limit int) (DevicePage, error) {
	return access.ListDevices(ctx, cursor, limit)
}
func (AccessDeviceStore) RemoveDevice(ctx context.Context, id string) error {
	return access.RemoveDevice(ctx, id)
}

const RegistrationPath = "/_magpie/remote/v1/registration"
const BrowserLoginPath = "/_magpie/remote/v1/browser-login"
const ProtocolVersion = 1

type Registration struct {
	ProtocolVersion int                  `json:"protocolVersion"`
	Device          RegisteredDevice     `json:"device"`
	Gateway         GatewayConnection    `json:"gateway"`
	Management      ManagementConnection `json:"management"`
	ServerVersion   string               `json:"serverVersion"`
}
type RegisteredDevice struct {
	ID string `json:"id"`
	Peer
}

// GatewayConnection is a credential delivery payload. Never log, diagnose or
// expose it through ordinary status or list APIs.
type GatewayConnection struct {
	BaseURL string `json:"baseUrl"`
	KeyID   string `json:"keyId"`
	Key     string `json:"key"`
}
type ManagementConnection struct {
	BaseURL  string `json:"baseUrl"`
	LoginURL string `json:"loginUrl"`
}

var (
	ErrInvalidRequest      = errors.New("invalid request")
	ErrIdentityRequired    = errors.New("Tailscale node identity required")
	ErrIdentityUnavailable = errors.New("Tailscale identity lookup unavailable")
)
