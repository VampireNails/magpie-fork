package remotejoin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/tailnetdns"
)

type ClientStatus struct {
	ServerURL          string    `json:"serverUrl"`
	ProviderID         string    `json:"providerId"`
	DeviceID           string    `json:"deviceId,omitempty"`
	ManagementURL      string    `json:"managementUrl,omitempty"`
	State              string    `json:"state"`
	ConnectionMode     string    `json:"connectionMode"`
	LastRegistrationAt time.Time `json:"lastRegistrationAt,omitempty"`
	LastSyncAt         time.Time `json:"lastSyncAt,omitempty"`
	LastErrorCode      string    `json:"lastErrorCode,omitempty"`
}
type Binding interface {
	Bind(id, root, key string) error
	Refresh(context.Context, string) error
}
type nativeBinding struct{}

func (nativeBinding) Bind(id, root, key string) error { return provider.BindRemote(id, root, key) }
func (nativeBinding) Refresh(ctx context.Context, id string) error {
	p, err := provider.Find(id)
	if err != nil {
		return err
	}
	return p.RefetchRemoteQuiet(ctx)
}

type ClientOptions struct {
	HTTPClient   *http.Client
	Binding      Binding
	Now          func() time.Time
	BeforeSwitch func(context.Context) error
}
type Client struct {
	http         *http.Client
	binding      Binding
	now          func() time.Time
	beforeSwitch func(context.Context) error
}

var connectionOperations = filelock.Mutex{Path: func() string { return connectionPath() + ".operation.lock" }}

func NewClient(o ClientOptions) *Client {
	client := &http.Client{}
	if o.HTTPClient != nil {
		*client = *o.HTTPClient
	} else {
		t := &http.Transport{}
		if base, ok := http.DefaultTransport.(*http.Transport); ok {
			t = base.Clone()
		}
		if t.TLSClientConfig != nil {
			t.TLSClientConfig = t.TLSClientConfig.Clone()
			t.TLSClientConfig.InsecureSkipVerify = false
		}
		t.Proxy = netproxy.Func
		client.Transport = netproxy.Dispatch(t)
	}
	client.Jar = nil
	client.Timeout = 20 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if o.Binding == nil {
		o.Binding = nativeBinding{}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	c := &Client{http: client, binding: o.Binding, now: o.Now, beforeSwitch: o.BeforeSwitch}
	if s, err := c.Status(context.Background()); err == nil && s.ServerURL != "" {
		setDNSTarget(s.ServerURL)
	} else {
		tailnetdns.SetTarget("")
	}
	return c
}

func setDNSTarget(root string) {
	u, err := url.Parse(root)
	if err == nil {
		tailnetdns.SetTarget(u.Host)
	}
}
func (c *Client) Configure(ctx context.Context, raw string) (ClientStatus, error) {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	root, err := RootOrigin(raw)
	if err != nil {
		return ClientStatus{}, err
	}
	u, _ := url.Parse(root)
	if net.ParseIP(u.Hostname()) != nil {
		if name, e := tailnetdns.NameForIP(ctx, u.Hostname()); e == nil {
			port := u.Port()
			u.Host = name
			if port != "" {
				u.Host = net.JoinHostPort(name, port)
			}
			root, err = RootOrigin(u.String())
			if err != nil {
				return ClientStatus{}, err
			}
		}
	}
	if err := connectionOperations.LockContext(ctx, filelock.Wait); err != nil {
		return ClientStatus{}, errors.New("connection operation lock unavailable")
	}
	defer connectionOperations.Unlock()
	var previous ClientStatus
	if err = withConnection(func(f *connectionFile) (bool, error) { previous = f.ClientStatus; return false, nil }); err != nil {
		return ClientStatus{}, err
	}
	if previous.ServerURL != "" && previous.ServerURL != root {
		if c.beforeSwitch == nil {
			return previous, errors.New("restore hook required before changing server")
		}
		if err = c.beforeSwitch(ctx); err != nil {
			return previous, errors.New("managed sessions could not be restored")
		}
	}
	err = withConnection(func(f *connectionFile) (bool, error) {
		if f.ClientStatus != previous {
			return false, errors.New("connection changed; retry")
		}
		if f.ProviderID == "" {
			var b [16]byte
			if _, e := rand.Read(b[:]); e != nil {
				return false, errors.New("could not create provider identity")
			}
			f.ProviderID = "remote-" + hex.EncodeToString(b[:])
		}
		if f.ServerURL != root {
			f.DeviceID = ""
			f.ManagementURL = ""
			f.LastSyncAt = time.Time{}
			f.LastRegistrationAt = time.Time{}
		}
		f.SchemaVersion = 1
		f.ServerURL = root
		f.ConnectionMode = "auto"
		f.State = "connecting"
		f.LastErrorCode = ""
		return true, nil
	})
	if err != nil {
		return ClientStatus{}, err
	}
	setDNSTarget(root)
	return c.refresh(ctx)
}
func (c *Client) Refresh(ctx context.Context) (ClientStatus, error) {
	if err := connectionOperations.LockContext(ctx, filelock.Wait); err != nil {
		return ClientStatus{}, errors.New("connection operation lock unavailable")
	}
	defer connectionOperations.Unlock()
	return c.refresh(ctx)
}
func (c *Client) Status(ctx context.Context) (ClientStatus, error) {
	if err := ctx.Err(); err != nil {
		return ClientStatus{}, err
	}
	var out ClientStatus
	err := withConnection(func(f *connectionFile) (bool, error) { out = f.ClientStatus; return false, nil })
	if err != nil {
		return ClientStatus{}, err
	}
	if out.ServerURL == "" {
		out.State = "unconfigured"
	}
	return out, nil
}
func (c *Client) ManagementURL(ctx context.Context) (string, error) {
	s, err := c.Status(ctx)
	if err != nil {
		return "", err
	}
	if s.State != "connected" || s.ManagementURL == "" {
		return "", errors.New("management entry unavailable")
	}
	return s.ManagementURL, nil
}
func (c *Client) refresh(ctx context.Context) (ClientStatus, error) {
	s, err := c.Status(ctx)
	if err != nil {
		return s, err
	}
	if s.ServerURL == "" {
		return s, errors.New("server is not configured")
	}
	setDNSTarget(s.ServerURL)
	fail := func(code string) (ClientStatus, error) {
		storeErr := withConnection(func(f *connectionFile) (bool, error) {
			if f.ServerURL != s.ServerURL || f.ProviderID != s.ProviderID {
				return false, nil
			}
			f.State = "error"
			f.LastErrorCode = code
			return true, nil
		})
		if storeErr != nil {
			return ClientStatus{}, storeErr
		}
		out, _ := c.Status(context.Background())
		return out, errors.New(code)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.ServerURL+RegistrationPath, bytes.NewBufferString("{}"))
	if err != nil {
		return fail("registration_request_invalid")
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, tailnetdns.ErrToolUnavailable) {
			return fail("tailscale_tool_unavailable")
		}
		if errors.Is(err, tailnetdns.ErrStatusUnavailable) {
			return fail("tailscale_status_unavailable")
		}
		if errors.Is(err, tailnetdns.ErrMappingUnavailable) {
			return fail("tailscale_mapping_unavailable")
		}
		var certErr *tls.CertificateVerificationError
		var unknown x509.UnknownAuthorityError
		var hostname x509.HostnameError
		if errors.As(err, &certErr) || errors.As(err, &unknown) || errors.As(err, &hostname) {
			return fail("tls_certificate_invalid")
		}
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) {
			return fail("dns_resolution_failed")
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return fail("registration_timeout")
		}
		return fail("registration_unreachable")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fail("registration_rejected")
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 65537))
	if err != nil || len(raw) > 65536 {
		return fail("registration_response_invalid")
	}
	var r Registration
	if json.Unmarshal(raw, &r) != nil || !validRegistration(s.ServerURL, r) {
		return fail("registration_response_invalid")
	}
	if err = c.binding.Bind(s.ProviderID, s.ServerURL, r.Gateway.Key); err != nil {
		return fail("provider_binding_failed")
	}
	err = withConnection(func(f *connectionFile) (bool, error) {
		if f.ServerURL != s.ServerURL || f.ProviderID != s.ProviderID {
			return false, errors.New("connection changed; retry")
		}
		f.DeviceID = r.Device.ID
		f.ManagementURL = r.Management.LoginURL
		f.LastRegistrationAt = c.now().UTC()
		f.State = "connecting"
		f.LastErrorCode = ""
		return true, nil
	})
	if err != nil {
		return ClientStatus{}, err
	}
	if err = c.binding.Refresh(ctx, s.ProviderID); err != nil {
		return fail("model_refresh_failed")
	}
	now := c.now().UTC()
	err = withConnection(func(f *connectionFile) (bool, error) {
		if f.ServerURL != s.ServerURL || f.ProviderID != s.ProviderID {
			return false, errors.New("connection changed; retry")
		}
		f.DeviceID = r.Device.ID
		f.ManagementURL = r.Management.LoginURL
		f.LastSyncAt = now
		f.LastErrorCode = ""
		f.State = "connected"
		return true, nil
	})
	if err != nil {
		return ClientStatus{}, err
	}
	return c.Status(context.Background())
}
func validRegistration(root string, r Registration) bool {
	id, idErr := hex.DecodeString(r.Device.ID)
	key := strings.TrimPrefix(r.Gateway.Key, access.Prefix)
	decodedKey, keyErr := hex.DecodeString(key)
	if r.ProtocolVersion != ProtocolVersion || !cleanText(r.ServerVersion, 128) || idErr != nil || len(id) != 12 || !validPeer(r.Device.Peer) || !strings.HasPrefix(r.Gateway.Key, access.Prefix) || keyErr != nil || len(decodedKey) != 24 || r.Gateway.KeyID != r.Device.ID {
		return false
	}
	if len(r.Device.ID) > 256 || len(r.Gateway.Key) > 4096 || strings.ContainsAny(r.Device.ID+r.Device.StableNodeID+r.Device.NodeID, "\r\n\x00") {
		return false
	}
	gateway, e := RootOrigin(r.Gateway.BaseURL)
	if e != nil || gateway != root {
		return false
	}
	management, e := RootOrigin(r.Management.BaseURL)
	return e == nil && management == root && r.Management.LoginURL == root+BrowserLoginPath
}
