package tailnetdns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/proc"
)

type dialFn func(context.Context, string, string) (net.Conn, error)

var ErrToolUnavailable = errors.New("tailscale executable unavailable")
var ErrStatusUnavailable = errors.New("tailscale status unavailable")
var ErrMappingUnavailable = errors.New("tailscale node mapping unavailable")

var target struct {
	sync.RWMutex
	authority string
}

// SetTarget restricts fallback to the explicitly configured server host.
func SetTarget(authority string) {
	target.Lock()
	target.authority = canonicalAuthority(authority)
	target.Unlock()
}
func canonicalAuthority(authority string) string {
	if authority == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		host, port = strings.Trim(authority, "[]"), "443"
	}
	return net.JoinHostPort(strings.TrimSuffix(strings.ToLower(host), "."), port)
}
func IsTarget(authority string) bool {
	target.RLock()
	defer target.RUnlock()
	return target.authority != "" && target.authority == canonicalAuthority(authority)
}

// Wrap uses system DNS first. A verified Tailscale status mapping is only
// consulted for a failed lookup of the configured host; TLS still uses the URL host.
func Wrap(d dialFn) dialFn {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := d(ctx, network, addr)
		if err == nil {
			return c, nil
		}
		host, port, e := net.SplitHostPort(addr)
		if e != nil || !IsTarget(addr) {
			return nil, err
		}
		var dns *net.DNSError
		if !errors.As(err, &dns) {
			return nil, err
		}
		ip, e := lookup(ctx, strings.TrimSuffix(strings.ToLower(host), "."))
		if e != nil {
			return nil, e
		}
		return d(ctx, network, net.JoinHostPort(ip, port))
	}
}

var lookup = lookupStatus

func lookupStatus(ctx context.Context, host string) (string, error) {
	nodes, err := statusNodes(ctx)
	if err != nil {
		return "", err
	}
	return match(nodes, host)
}

// NameForIP only returns a certificate hostname from local Tailscale state.
func NameForIP(ctx context.Context, raw string) (string, error) {
	ip, err := netip.ParseAddr(raw)
	if err != nil || !tailIP(ip) {
		return "", ErrMappingUnavailable
	}
	nodes, err := statusNodes(ctx)
	if err != nil {
		return "", err
	}
	return nameForIP(nodes, ip)
}

func nameForIP(nodes []node, ip netip.Addr) (string, error) {
	var name string
	for _, n := range nodes {
		candidate := strings.TrimSuffix(strings.ToLower(n.DNSName), ".")
		if !strings.HasSuffix(candidate, ".ts.net") {
			continue
		}
		for _, address := range n.TailscaleIPs {
			v, e := netip.ParseAddr(address)
			if e == nil && v == ip {
				if name != "" && name != candidate {
					return "", ErrMappingUnavailable
				}
				name = candidate
			}
		}
	}
	if name == "" {
		return "", ErrMappingUnavailable
	}
	if mapped, e := match(nodes, name); e != nil || mapped != ip.String() {
		return "", ErrMappingUnavailable
	}
	return name, nil
}

func statusNodes(ctx context.Context) ([]node, error) {
	path := proc.FindTool("tailscale")
	if path == "" && runtime.GOOS == "windows" {
		for _, base := range proc.SystemDirs(os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")) {
			candidate := filepath.Join(base, "Tailscale", "tailscale.exe")
			if s, e := os.Stat(candidate); e == nil && !s.IsDir() {
				path = candidate
				break
			}
		}
	}
	if path == "" {
		return nil, ErrToolUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := proc.CommandContext(ctx, path, "status", "--json")
	var b boundedBuffer
	cmd.Stdout = &b
	cmd.Stderr = io.Discard
	if cmd.Run() != nil || b.exceeded {
		return nil, ErrStatusUnavailable
	}
	var status struct {
		BackendState string          `json:"BackendState"`
		Self         node            `json:"Self"`
		Peer         map[string]node `json:"Peer"`
	}
	if json.Unmarshal(b.Bytes(), &status) != nil || status.BackendState != "Running" {
		return nil, ErrStatusUnavailable
	}
	nodes := []node{status.Self}
	for _, n := range status.Peer {
		nodes = append(nodes, n)
	}
	return nodes, nil
}

type node struct {
	DNSName      string   `json:"DNSName"`
	TailscaleIPs []string `json:"TailscaleIPs"`
}

func match(nodes []node, host string) (string, error) {
	var found string
	for _, n := range nodes {
		name := strings.TrimSuffix(strings.ToLower(n.DNSName), ".")
		if name != host || !strings.HasSuffix(name, ".ts.net") {
			continue
		}
		for _, raw := range n.TailscaleIPs {
			ip, e := netip.ParseAddr(raw)
			if e != nil || !tailIP(ip) {
				continue
			}
			if found != "" && found != ip.String() {
				return "", ErrMappingUnavailable
			}
			found = ip.String()
			break
		}
	}
	if found == "" {
		return "", ErrMappingUnavailable
	}
	return found, nil
}
func tailIP(ip netip.Addr) bool {
	return netip.MustParsePrefix("100.64.0.0/10").Contains(ip) || netip.MustParsePrefix("fd7a:115c:a1e0::/48").Contains(ip)
}

type boundedBuffer struct {
	buf      bytes.Buffer
	exceeded bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > 4<<20 {
		b.exceeded = true
		return 0, errors.New("status too large")
	}
	return b.buf.Write(p)
}

func (b *boundedBuffer) Bytes() []byte { return b.buf.Bytes() }
