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
		ips, e := lookup(ctx, strings.TrimSuffix(strings.ToLower(host), "."))
		if e != nil {
			return nil, e
		}
		var last error = ErrMappingUnavailable
		attempts := 0
		for _, raw := range ips {
			ip, e := netip.ParseAddr(raw)
			if e != nil || (network == "tcp4" && !ip.Is4()) || (network == "tcp6" && !ip.Is6()) {
				continue
			}
			if attempts >= 16 {
				break
			}
			attempts++
			c, last = d(ctx, network, net.JoinHostPort(raw, port))
			if last == nil {
				return c, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		return nil, last
	}
}

var lookup = lookupStatus

func lookupStatus(ctx context.Context, host string) ([]string, error) {
	nodes, err := statusNodes(ctx)
	if err != nil {
		return nil, err
	}
	return matchAll(nodes, host)
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
	owners := 0
	for _, n := range nodes {
		for _, raw := range n.TailscaleIPs {
			v, err := netip.ParseAddr(raw)
			if err == nil && v == ip {
				owners++
				name = strings.TrimSuffix(strings.ToLower(n.DNSName), ".")
				break
			}
		}
	}
	if owners != 1 || !strings.HasSuffix(name, ".ts.net") {
		return "", ErrMappingUnavailable
	}
	ips, err := matchAll(nodes, name)
	if err != nil {
		return "", err
	}
	for _, raw := range ips {
		if raw == ip.String() {
			return name, nil
		}
	}
	return "", ErrMappingUnavailable
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
	ips, err := matchAll(nodes, host)
	if err != nil {
		return "", err
	}
	return ips[0], nil
}

func matchAll(nodes []node, host string) ([]string, error) {
	var owner *node
	for i := range nodes {
		name := strings.TrimSuffix(strings.ToLower(nodes[i].DNSName), ".")
		if name != host || !strings.HasSuffix(name, ".ts.net") {
			continue
		}
		if owner != nil {
			return nil, ErrMappingUnavailable
		}
		owner = &nodes[i]
	}
	if owner == nil {
		return nil, ErrMappingUnavailable
	}
	var ips []string
	for _, raw := range owner.TailscaleIPs {
		ip, err := netip.ParseAddr(raw)
		if err != nil || !tailIP(ip) {
			continue
		}
		for i := range nodes {
			if &nodes[i] == owner {
				continue
			}
			for _, other := range nodes[i].TailscaleIPs {
				v, e := netip.ParseAddr(other)
				if e == nil && v == ip {
					return nil, ErrMappingUnavailable
				}
			}
		}
		ips = append(ips, ip.String())
	}
	if len(ips) == 0 {
		return nil, ErrMappingUnavailable
	}
	return ips, nil
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
