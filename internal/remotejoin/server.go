package remotejoin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
)

type ServerConfig struct {
	BaseURL       string
	ServerVersion string
	Resolver      PeerResolver
	Store         DeviceStore
	// WebSessionReady must check the currently active native Web signing key.
	// It returns no key. A nil or failing callback prevents bootstrap success.
	WebSessionReady func(context.Context) error
}

func NewRegistrationHandler(config ServerConfig) (http.Handler, error) {
	origin, err := RootOrigin(config.BaseURL)
	if err != nil {
		return nil, err
	}
	if !cleanText(config.ServerVersion, 128) {
		return nil, errors.New("server version is required")
	}
	config.BaseURL = origin
	if config.Resolver == nil {
		config.Resolver = NewLocalAPIResolver(nil, 0)
	}
	if config.Store == nil {
		config.Store = AccessDeviceStore{}
	}
	return &registrationServer{config}, nil
}

type registrationServer struct{ config ServerConfig }

func (s *registrationServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path != RegistrationPath || r.URL.RawPath != "" {
		writeError(w, 404, "INVALID_REQUEST", "Unknown registration endpoint")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, 405, "INVALID_REQUEST", "Use POST with a JSON registration object")
		return
	}
	if r.Context().Value(serveContextKey{}) != true {
		writePeerError(w, ErrIdentityRequired)
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery || !sameOriginRequest(r, s.config.BaseURL) {
		writeError(w, 400, "INVALID_REQUEST", "Use the configured server origin without query parameters")
		return
	}
	if !registrationInput(w, r) {
		return
	}
	p, err := s.config.Resolver.Resolve(r.Context(), r)
	if err != nil {
		writePeerError(w, err)
		return
	}
	if !validPeer(p) {
		writePeerError(w, ErrIdentityRequired)
		return
	}
	// Check the native management signer before issuing a gateway credential.
	// The callback must check the live signer, including rotations, on each call.
	if s.config.WebSessionReady == nil || s.config.WebSessionReady(r.Context()) != nil {
		writeError(w, 503, "WEB_SESSION_UNAVAILABLE", "Server management sign-in is temporarily unavailable; retry shortly")
		return
	}
	credential, err := s.config.Store.EnsureDevice(r.Context(), p)
	if err != nil || credential.Device.ID == "" || credential.Secret == "" || credential.Device.StableNodeID != p.StableNodeID || credential.Device.NodeID != p.NodeID {
		writeError(w, 503, "STORE_UNAVAILABLE", "Device registration storage is temporarily unavailable; retry shortly")
		return
	}
	// The signer might have become unavailable while waiting for the store.
	if s.config.WebSessionReady(r.Context()) != nil {
		writeError(w, 503, "WEB_SESSION_UNAVAILABLE", "Server management sign-in is temporarily unavailable; retry shortly")
		return
	}
	result := Registration{
		ProtocolVersion: ProtocolVersion,
		Device:          RegisteredDevice{ID: credential.Device.ID, Peer: p},
		Gateway:         GatewayConnection{BaseURL: s.config.BaseURL, KeyID: credential.Device.ID, Key: credential.Secret},
		Management:      ManagementConnection{BaseURL: s.config.BaseURL, LoginURL: s.config.BaseURL + BrowserLoginPath},
		ServerVersion:   s.config.ServerVersion,
	}
	_ = json.NewEncoder(w).Encode(result)
}

func registrationInput(w http.ResponseWriter, r *http.Request) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || r.Header.Get("Content-Encoding") != "" {
		writeError(w, 400, "INVALID_REQUEST", "Send an uncompressed application/json object")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		writeError(w, 400, "INVALID_REQUEST", "Registration body must be at most 4096 bytes")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		writeError(w, 400, "INVALID_REQUEST", "Send a JSON object with optional clientVersion only")
		return false
	}
	seen := false
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil || key != "clientVersion" || seen {
			writeError(w, 400, "INVALID_REQUEST", "Only one optional clientVersion field is accepted; identity and agent fields are not accepted")
			return false
		}
		seen = true
		value, err := decoder.Token()
		version, ok := value.(string)
		if err != nil || !ok || !cleanText(version, 128) {
			writeError(w, 400, "INVALID_REQUEST", "clientVersion must be a nonempty string of at most 128 characters")
			return false
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		writeError(w, 400, "INVALID_REQUEST", "Registration JSON is incomplete")
		return false
	}
	if _, err = decoder.Token(); err != io.EOF {
		writeError(w, 400, "INVALID_REQUEST", "Send exactly one registration JSON object")
		return false
	}
	return true
}

func sameOriginRequest(r *http.Request, origin string) bool {
	values := r.Header.Values("Origin")
	if len(values) == 0 {
		return true
	}
	if len(values) != 1 {
		return false
	}
	got, err := RootOrigin(values[0])
	return err == nil && got == origin
}

func writePeerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidRequest):
		writeError(w, 400, "INVALID_REQUEST", "A single valid Tailscale source IP is required")
	case errors.Is(err, ErrIdentityRequired):
		writeError(w, 401, "TAILSCALE_IDENTITY_REQUIRED", "A verified Tailscale node identity is required on the protected Serve listener")
	default:
		writeError(w, 503, "TAILSCALE_IDENTITY_UNAVAILABLE", "Tailscale identity lookup is temporarily unavailable; retry shortly")
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.WriteHeader(status)
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	payload.Error.Code, payload.Error.Message = code, message
	_ = json.NewEncoder(w).Encode(payload)
}

// RootOrigin validates a configured HTTPS root and returns a canonical origin.
// The error deliberately excludes the input, which may contain a credential.
func RootOrigin(raw string) (string, error) {
	invalid := errors.New("server URL must be an HTTPS root origin without credentials, path, query or fragment")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" || strings.ContainsAny(raw, "?#\\") || u.ForceQuery {
		return "", invalid
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", invalid
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" {
			return "", invalid
		}
	} else {
		if strings.ContainsAny(u.Host, "[]") {
			return "", invalid
		}
		if len(host) > 253 {
			return "", invalid
		}
		for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return "", invalid
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					return "", invalid
				}
			}
		}
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", invalid
		}
		port = strconv.Itoa(n)
	} else if strings.HasSuffix(u.Host, ":") {
		return "", invalid
	}
	if port == "443" {
		port = ""
	}
	authority := host
	if port != "" {
		authority = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		authority = "[" + host + "]"
	}
	return "https://" + authority, nil
}

// ValidateReturnPath accepts only a clean absolute site path. Query parameters,
// fragments and escapes are excluded so login links cannot carry credentials.
// GUI owns the actual cookie issuance and redirect using this validated path.
func ValidateReturnPath(raw string) (string, error) {
	if raw == "" {
		return "/", nil
	}
	if !cleanText(raw, 2048) || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.ContainsAny(raw, "%?#\\") || path.Clean(raw) != raw {
		return "", errors.New("return path must be a clean absolute site path")
	}
	return raw, nil
}
