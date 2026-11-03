package remotejoin

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/steady"
)

type connectionFile struct {
	SchemaVersion int `json:"schemaVersion"`
	ClientStatus
}

func connectionPath() string { return filepath.Join(appdir.Config(), "remote-connection.json") }

var connectionStorage = filelock.Mutex{Path: func() string { return connectionPath() + ".lock" }}

func withConnection(fn func(*connectionFile) (bool, error)) error {
	return withConnectionContext(context.Background(), fn)
}

func withConnectionContext(ctx context.Context, fn func(*connectionFile) (bool, error)) error {
	path := connectionPath()
	if err := connectionStorage.LockContext(ctx, 10*time.Second); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("connection storage lock unavailable")
	}
	defer connectionStorage.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	var f connectionFile
	data, err := steady.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("connection storage unreadable")
	}
	if err == nil {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&f) != nil || decoder.Decode(new(any)) != io.EOF || f.SchemaVersion != 1 || f.ServerURL == "" || f.ProviderID == "" {
			return errors.New("connection storage invalid")
		}
		if origin, e := RootOrigin(f.ServerURL); e != nil || origin != f.ServerURL {
			return errors.New("connection storage invalid")
		}
		if f.ManagementURL != "" && f.ManagementURL != f.ServerURL+BrowserLoginPath {
			return errors.New("connection storage invalid")
		}
		if f.State == "connected" && (f.ManagementURL == "" || f.DeviceID == "") {
			return errors.New("connection storage invalid")
		}
		if len(f.ProviderID) != len("remote-")+32 || f.ProviderID[:7] != "remote-" {
			return errors.New("connection storage invalid")
		}
		if _, e := hex.DecodeString(f.ProviderID[7:]); e != nil {
			return errors.New("connection storage invalid")
		}
		if f.DeviceID != "" {
			id, e := hex.DecodeString(f.DeviceID)
			if e != nil || len(id) != 12 {
				return errors.New("connection storage invalid")
			}
		}
		switch f.State {
		case "connecting", "connected", "error":
		default:
			return errors.New("connection storage invalid")
		}
		if f.ConnectionMode != "auto" && f.ConnectionMode != "paused" {
			return errors.New("connection storage invalid")
		}
		switch f.LastErrorCode {
		case "", "registration_request_invalid", "registration_unreachable", "registration_rejected", "registration_response_invalid", "provider_binding_failed", "model_refresh_failed", "tailscale_tool_unavailable", "tailscale_status_unavailable", "tailscale_mapping_unavailable", "tls_certificate_invalid", "dns_resolution_failed", "registration_timeout":
		default:
			return errors.New("connection storage invalid")
		}
	}
	changed, err := fn(&f)
	if err != nil || !changed {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err = json.MarshalIndent(f, "", "  ")
	if err != nil {
		return errors.New("connection storage encoding failed")
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.New("connection storage unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err = steady.WriteFile(path, append(data, '\n'), 0600); err != nil {
		return errors.New("connection storage write failed")
	}
	return nil
}
