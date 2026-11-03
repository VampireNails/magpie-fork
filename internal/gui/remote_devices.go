package gui

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/yetone/magpie/internal/access"
)

// remoteDeviceRoutes installs device management on the supplied mux. The
// caller must put that mux behind the native Web cookie and origin guards.
func remoteDeviceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/remote-devices", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !deviceSameOrigin(r) {
			deviceError(w, http.StatusForbidden, "INVALID_ORIGIN", "Invalid origin")
			return
		}
		q, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || !deviceQueryOK(q) {
			deviceError(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid device request")
			return
		}
		limit := 50
		if q.Has("limit") {
			limit, err = strconv.Atoi(q.Get("limit"))
			if err != nil || limit < 1 || limit > 100 || strconv.Itoa(limit) != q.Get("limit") {
				deviceError(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid device request")
				return
			}
		}
		cursor := ""
		if q.Has("cursor") {
			decoded, decodeErr := base64.RawURLEncoding.DecodeString(q.Get("cursor"))
			if decodeErr != nil || !deviceIDOK(string(decoded)) || base64.RawURLEncoding.EncodeToString(decoded) != q.Get("cursor") {
				deviceError(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid device request")
				return
			}
			cursor = string(decoded)
		}
		page, err := access.ListDevices(r.Context(), cursor, limit)
		if err != nil {
			deviceError(w, http.StatusServiceUnavailable, "STORE_UNAVAILABLE", "Device store unavailable")
			return
		}
		if page.NextCursor != nil {
			next := base64.RawURLEncoding.EncodeToString([]byte(*page.NextCursor))
			page.NextCursor = &next
		}
		writeJSON(w, page)
	})
	mux.HandleFunc("DELETE /api/remote-devices/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !deviceSameOrigin(r) {
			deviceError(w, http.StatusForbidden, "INVALID_ORIGIN", "Invalid origin")
			return
		}
		id := r.PathValue("id")
		if !deviceIDOK(id) || r.URL.RawQuery != "" || r.ContentLength != 0 {
			deviceError(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid device request")
			return
		}
		if err := access.RemoveDevice(r.Context(), id); err != nil {
			deviceError(w, http.StatusServiceUnavailable, "STORE_UNAVAILABLE", "Device store unavailable")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func deviceIDOK(id string) bool {
	if len(id) != 24 {
		return false
	}
	for _, c := range id {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func deviceQueryOK(q url.Values) bool {
	for k, v := range q {
		if (k != "cursor" && k != "limit") || len(v) != 1 {
			return false
		}
	}
	return true
}

// deviceSameOrigin checks browser-supplied origins against the trusted request
// authority, including the public URL set by the protected Serve entry.
func deviceSameOrigin(r *http.Request) bool {
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

func deviceError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}
