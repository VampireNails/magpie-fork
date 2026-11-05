package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/gateway"
)

// remoteClientStatus contains only information safe for the desktop page.
type remoteClientStatus struct {
	ServerURL      string     `json:"serverUrl"`
	State          string     `json:"state"`
	Owner          *bool      `json:"owner"`
	Connected      *bool      `json:"connected"`
	LastSyncAt     *time.Time `json:"lastSyncAt"`
	ErrorCode      string     `json:"errorCode"`
	RecoveryTarget string     `json:"recoveryTarget"`
}

type remoteClientService interface {
	Status(context.Context) (remoteClientStatus, error)
	Connect(context.Context, string) (remoteClientStatus, error)
	Refresh(context.Context) (remoteClientStatus, error)
	Disconnect(context.Context) (remoteClientStatus, error)
	ManagementURL(context.Context) (string, error)
}

// localRemoteHandler is installed only by a desktop host with a real service.
func localRemoteHandler(next http.Handler, service remoteClientService) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", next)
	mux.HandleFunc("GET /api/remote-connection", func(w http.ResponseWriter, r *http.Request) {
		s, err := service.Status(r.Context())
		remoteStatus(w, s, err)
	})
	mux.HandleFunc("POST /api/remote-connection", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ServerURL remoteOptionalString `json:"serverUrl"`
		}
		if !remoteBody(w, r, &in) {
			return
		}
		s, err := service.Connect(r.Context(), string(in.ServerURL))
		remoteStatus(w, s, err)
	})
	for _, action := range []string{"refresh", "disconnect", "manage"} {
		mux.HandleFunc("POST /api/remote-connection/"+action, func(w http.ResponseWriter, r *http.Request) {
			var in struct{}
			if !remoteBody(w, r, &in) {
				return
			}
			switch action {
			case "refresh":
				s, err := service.Refresh(r.Context())
				remoteStatus(w, s, err)
			case "disconnect":
				s, err := service.Disconnect(r.Context())
				remoteStatus(w, s, err)
			case "manage":
				raw, err := service.ManagementURL(r.Context())
				status, statusErr := service.Status(r.Context())
				root, rootErr := url.Parse(status.ServerURL)
				u, parseErr := url.Parse(raw)
				if err != nil || statusErr != nil || rootErr != nil || root.Scheme != "https" || root.Host == "" || root.User != nil || root.RawQuery != "" || root.Fragment != "" || (root.Path != "" && root.Path != "/") || parseErr != nil || !strings.EqualFold(u.Host, root.Host) || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "/_magpie/remote/v1/browser-login" {
					remoteError(w, http.StatusServiceUnavailable, "UNAVAILABLE")
					return
				}
				remoteJSON(w, map[string]string{"url": u.String()})
			}
		})
	}
	return noFramedAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/remote-connection") && !remoteSameOrigin(r) {
			remoteError(w, 403, "INVALID_ORIGIN")
			return
		}
		mux.ServeHTTP(w, r)
	}))
}

func remoteBody(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			remoteError(w, 413, "REQUEST_TOO_LARGE")
		} else {
			remoteError(w, 400, "INVALID_REQUEST")
		}
		return false
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		remoteError(w, 400, "INVALID_REQUEST")
		return false
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		remoteError(w, 400, "INVALID_REQUEST")
		return false
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		remoteError(w, 400, "INVALID_REQUEST")
		return false
	}
	return true
}
func remoteStatus(w http.ResponseWriter, s remoteClientStatus, err error) {
	if err != nil {
		switch s.ErrorCode {
		case "OWNER_CONFLICT", "RESTORE_FAILED", "registration_request_invalid", "tailscale_tool_unavailable", "tailscale_status_unavailable", "tailscale_mapping_unavailable", "tls_certificate_invalid", "dns_resolution_failed", "registration_timeout", "registration_unreachable", "registration_rejected", "registration_response_invalid", "registration_failed", "connection_storage_unavailable", "provider_binding_failed", "model_refresh_failed":
			remoteError(w, 503, s.ErrorCode)
		default:
			remoteError(w, 503, "UNAVAILABLE")
		}
		return
	}
	// Never serialize arbitrary service strings into fields that describe state.
	switch s.State {
	case "unconfigured", "connecting", "connected", "retrying", "restoring", "offline-detached", "paused", "restore-failed", "unknown":
	default:
		s.State = "unknown"
	}
	switch s.RecoveryTarget {
	case "", "workbuddy", "hermes":
	default:
		s.RecoveryTarget = ""
	}
	switch s.ErrorCode {
	case "", "OWNER_CONFLICT", "RESTORE_FAILED", "UNAVAILABLE", "registration_request_invalid", "tailscale_tool_unavailable", "tailscale_status_unavailable", "tailscale_mapping_unavailable", "tls_certificate_invalid", "dns_resolution_failed", "registration_timeout", "registration_unreachable", "registration_rejected", "registration_response_invalid", "registration_failed", "connection_storage_unavailable", "provider_binding_failed", "model_refresh_failed":
	default:
		s.ErrorCode = "UNAVAILABLE"
	}
	u, err := url.Parse(s.ServerURL)
	if err != nil || (s.ServerURL != "" && (u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/"))) {
		s.ServerURL = ""
	}
	remoteJSON(w, s)
}
func remoteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func remoteError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": strings.ToLower(strings.ReplaceAll(code, "_", " "))}})
}

// remoteSameOrigin checks browser-supplied origins without using them to select the host role.
func remoteSameOrigin(r *http.Request) bool {
	scheme, _ := r.Context().Value(windowSchemeKey{}).(string)
	if scheme == "" {
		scheme = r.URL.Scheme
	}
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	for _, name := range []string{"Origin", "Referer"} {
		values := r.Header.Values(name)
		if len(values) > 1 {
			return false
		}
		if len(values) == 0 {
			continue
		}
		u, err := url.Parse(values[0])
		if err != nil || u.User != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host) || u.Scheme != scheme {
			return false
		}
	}
	return true
}

// desktopRemoteHandler binds the local control routes and boot flag together.
func desktopRemoteHandler(w Windows, gw *gateway.Server, service remoteClientService) http.Handler {
	next := localRemoteHandler(pageHandler(w, gw, true), service)
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/remote-connection") {
			if _, ok := r.Context().Value(windowSchemeKey{}).(string); !ok {
				remoteError(rw, http.StatusForbidden, "UNAVAILABLE")
				return
			}
		}
		next.ServeHTTP(rw, r)
	})
}

// remoteOptionalString accepts an absent field or a JSON string, never null.
type remoteOptionalString string

func (s *remoteOptionalString) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || data[0] != '"' {
		return errors.New("string required")
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*s = remoteOptionalString(value)
	return nil
}
