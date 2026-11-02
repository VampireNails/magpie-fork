package provider

import (
	"errors"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/steady"
)

var remoteBindingMu sync.Mutex

// BindRemote updates only the connection fields of the native Remote provider.
// The caller persists its random id before calling; it must never use Add's
// next-free-id behavior when a device is registered again.
func BindRemote(id, root, key string) error {
	if id == "" || id != Slug(id) || root == "" || key == "" {
		return errors.New("invalid remote binding")
	}
	remoteBindingMu.Lock()
	defer remoteBindingMu.Unlock()
	unlock, held := filelock.Lock(Path()+".remote-binding.lock", 10*time.Second)
	if !held {
		return errors.New("remote binding lock unavailable")
	}
	defer unlock()
	b, err := steady.ReadFile(Path())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("provider store unreadable")
	}
	if err == nil {
		if _, err := parseProviders(b); err != nil {
			return errors.New("provider store invalid")
		}
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
		if p.ModelsURL != "" || len(p.Keys) != 0 || len(p.Headers) != 0 || p.Proxy != "" || len(p.AccountProxies) != 0 {
			return errors.New("remote provider has unsupported connection overrides")
		}
		p.Key, p.Chat, p.Responses, p.Anthropic, p.Decide = key, root, "", "", ""
		p.InsecureTLS = false
		f.Providers[i] = normalize(p)
	} else {
		f.Providers = append(f.Providers, normalize(Provider{ID: id, Name: "Remote magpie", Preset: RemoteMagpiePreset, Chat: root, Key: key}))
	}
	return storeNotifying(f, false)
}
