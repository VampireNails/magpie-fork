package remotejoin

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type serveContextKey struct{}

// TrustedServeConnContext must be installed only on a dedicated, protected
// Tailscale Serve listener, after the host has verified its socket permissions
// and accepted peer. Never install it on a TCP or general loopback listener.
// Chain any existing ConnContext before calling this function.
func TrustedServeConnContext(ctx context.Context, _ net.Conn) context.Context {
	return context.WithValue(ctx, serveContextKey{}, true)
}

type LocalAPIResolver struct {
	client  *http.Client
	timeout time.Duration
}

// NewLocalAPIResolver copies client configuration; its transport must be safe
// for concurrent use. Redirects are never followed. A nil client uses only the
// local tailscaled Unix socket (Linux server), never an HTTP proxy.
func NewLocalAPIResolver(client *http.Client, timeout time.Duration) *LocalAPIResolver {
	if client == nil {
		client = unixLocalAPIClient("/var/run/tailscale/tailscaled.sock")
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &LocalAPIResolver{client: &copy, timeout: timeout}
}

// NewUnixLocalAPIResolver uses only the given tailscaled socket. The host owns
// its path and permissions; this client never uses TCP or environment proxies.
func NewUnixLocalAPIResolver(socket string, timeout time.Duration) *LocalAPIResolver {
	return NewLocalAPIResolver(unixLocalAPIClient(socket), timeout)
}

func unixLocalAPIClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 16 << 10,
	}}
}

func (r *LocalAPIResolver) Resolve(ctx context.Context, req *http.Request) (Peer, error) {
	if req.Context().Value(serveContextKey{}) != true {
		return Peer{}, ErrIdentityRequired
	}
	values := req.Header.Values("X-Forwarded-For")
	if len(values) == 0 {
		return Peer{}, ErrIdentityRequired
	}
	if len(values) != 1 {
		return Peer{}, ErrInvalidRequest
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(values[0]))
	if err != nil || ip.Zone() != "" || !ip.IsGlobalUnicast() {
		return Peer{}, ErrInvalidRequest
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	lookup, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://local-tailscaled.sock/localapi/v0/whois?addr="+url.QueryEscape(ip.String()), nil)
	res, err := r.client.Do(lookup)
	if err != nil {
		return Peer{}, ErrIdentityUnavailable
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return Peer{}, ErrIdentityRequired
	}
	if res.StatusCode != http.StatusOK {
		return Peer{}, ErrIdentityUnavailable
	}
	const maxWhois = 256 << 10
	body, err := io.ReadAll(io.LimitReader(res.Body, maxWhois+1))
	if err != nil || len(body) > maxWhois {
		return Peer{}, ErrIdentityUnavailable
	}
	var who struct {
		Node *struct {
			StableID     string
			ID           json.Number
			ComputedName string
			Name         string
		}
	}
	if json.Unmarshal(body, &who) != nil {
		return Peer{}, ErrIdentityUnavailable
	}
	if who.Node == nil {
		return Peer{}, ErrIdentityRequired
	}
	n := who.Node
	id, err := n.ID.Int64()
	if err != nil || id <= 0 {
		return Peer{}, ErrIdentityRequired
	}
	name := n.ComputedName
	if name == "" {
		name = strings.TrimSuffix(n.Name, ".")
	}
	p := Peer{StableNodeID: n.StableID, NodeID: strconv.FormatInt(id, 10), DisplayName: name}
	if !validPeer(p) {
		return Peer{}, ErrIdentityRequired
	}
	return p, nil
}

func cleanText(s string, max int) bool {
	return s != "" && strings.TrimSpace(s) == s && utf8.ValidString(s) && utf8.RuneCountInString(s) <= max && !strings.ContainsFunc(s, unicode.IsControl)
}

func validPeer(p Peer) bool {
	id, err := strconv.ParseInt(p.NodeID, 10, 64)
	return cleanText(p.StableNodeID, 256) && cleanText(p.DisplayName, 120) && err == nil && id > 0 && strconv.FormatInt(id, 10) == p.NodeID
}
