//go:build windows

package remotejoin

// On Windows the open config directory handle prevents its rename or removal.
type stableOwner struct{}

func acquireStableOwner(string) (*stableOwner, error) { return nil, nil }
func releaseStableOwner(*stableOwner)                 {}
func verifyOwnerPath(string) error                    { return nil }
