package provider

import (
	"errors"
	"slices"
)

// BindRemote updates only the connection fields of the native Remote provider.
// The caller persists its random id before calling; it must never use Add's
// next-free-id behavior when a device is registered again.
func BindRemote(id, root, key string) error {
	if id == "" || id != Slug(id) || root == "" || key == "" {
		return errors.New("invalid remote binding")
	}
	f, err := read()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(f.Providers, func(p Provider) bool { return p.ID == id })
	if i >= 0 {
		if !f.Providers[i].IsRemoteMagpie() {
			return errors.New("remote provider id already in use")
		}
		p := f.Providers[i]
		p.Key, p.Chat, p.Responses, p.Anthropic, p.Decide = key, root, "", "", ""
		f.Providers[i] = normalize(p)
	} else {
		f.Providers = append(f.Providers, normalize(Provider{ID: id, Name: "Remote magpie", Preset: RemoteMagpiePreset, Chat: root, Key: key}))
	}
	return store(f)
}
