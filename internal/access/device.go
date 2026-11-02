package access

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type DeviceIdentity struct{ StableNodeID, NodeID, DisplayName string }
type DeviceMetadata struct {
	StableNodeID string    `json:"stableNodeId"`
	NodeID       string    `json:"nodeId"`
	CreatedAt    time.Time `json:"createdAt"`
	LastSeenAt   time.Time `json:"lastSeenAt"`
}
type Device struct {
	ID           string    `json:"id"`
	DisplayName  string    `json:"displayName"`
	StableNodeID string    `json:"stableNodeId"`
	NodeID       string    `json:"nodeId"`
	CreatedAt    time.Time `json:"createdAt"`
	LastSeenAt   time.Time `json:"lastSeenAt"`
}
type DeviceCredential struct {
	Device Device
	Secret string
}
type DevicePage struct {
	Items      []Device `json:"items"`
	NextCursor *string  `json:"nextCursor"`
}

// EnsureDevice returns the secret exclusively for trusted registration.
func EnsureDevice(ctx context.Context, id DeviceIdentity) (DeviceCredential, error) {
	if err := ctx.Err(); err != nil {
		return DeviceCredential{}, err
	}
	id.StableNodeID = strings.TrimSpace(id.StableNodeID)
	id.DisplayName = strings.TrimSpace(id.DisplayName)
	if id.StableNodeID == "" || id.NodeID == "" || id.DisplayName == "" || utf8.RuneCountInString(id.DisplayName) > 120 {
		return DeviceCredential{}, errors.New("invalid device identity")
	}
	mu.Lock()
	defer mu.Unlock()
	if err := ctx.Err(); err != nil {
		return DeviceCredential{}, err
	}
	keys, err := load()
	if err != nil {
		return DeviceCredential{}, err
	}
	index := -1
	for i := range keys {
		if keys[i].Device != nil && keys[i].Device.StableNodeID == id.StableNodeID {
			if index >= 0 {
				return DeviceCredential{}, errors.New("duplicate device identity")
			}
			index = i
		}
	}
	now := time.Now().UTC()
	if index < 0 {
		keyID, e := random(12)
		if e != nil {
			return DeviceCredential{}, e
		}
		token, e := random(24)
		if e != nil {
			return DeviceCredential{}, e
		}
		keys = append(keys, Key{ID: keyID, Secret: Prefix + token, Device: &DeviceMetadata{StableNodeID: id.StableNodeID, CreatedAt: now}})
		index = len(keys) - 1
	}
	k := &keys[index]
	if k.Secret == "" {
		return DeviceCredential{}, errors.New("device credential missing")
	}
	k.Name = id.DisplayName
	k.Device.NodeID = id.NodeID
	k.Device.LastSeenAt = now
	k.Off = false
	k.ReadOnly = false
	k.Models = nil
	k.Accounts = nil
	k.Limit = nil
	k.More = nil
	k.Expires = time.Time{}
	k.Issuer = ""
	if err = save(keys); err != nil {
		return DeviceCredential{}, err
	}
	return DeviceCredential{Device: deviceOf(*k), Secret: k.Secret}, nil
}
func deviceOf(k Key) Device {
	return Device{ID: k.ID, DisplayName: k.Name, StableNodeID: k.Device.StableNodeID, NodeID: k.Device.NodeID, CreatedAt: k.Device.CreatedAt, LastSeenAt: k.Device.LastSeenAt}
}

// ListDevices returns only device metadata, ordered by key ID.
func ListDevices(ctx context.Context, cursor string, limit int) (DevicePage, error) {
	if err := ctx.Err(); err != nil {
		return DevicePage{}, err
	}
	if limit < 1 || limit > 100 {
		return DevicePage{}, errors.New("invalid page limit")
	}
	mu.Lock()
	defer mu.Unlock()
	if err := ctx.Err(); err != nil {
		return DevicePage{}, err
	}
	keys, err := load()
	if err != nil {
		return DevicePage{}, err
	}
	items := make([]Device, 0)
	for _, k := range keys {
		if k.Device != nil && k.ID > cursor {
			items = append(items, deviceOf(k))
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	page := DevicePage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		next := page.Items[len(page.Items)-1].ID
		page.NextCursor = &next
	}
	return page, nil
}

// RemoveDevice deletes only a device key and succeeds if absent.
func RemoveDevice(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	keys, err := load()
	if err != nil {
		return err
	}
	for i, k := range keys {
		if k.ID == id && k.Device != nil {
			keys = append(keys[:i], keys[i+1:]...)
			return save(keys)
		}
	}
	return nil
}
