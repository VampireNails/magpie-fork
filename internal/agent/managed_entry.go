package agent

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/yetone/magpie/internal/appdir"
)

// ErrManagedOwnerRequired prevents a separate CLI process from silently
// bypassing the resident remote client's recovery transaction.
var ErrManagedOwnerRequired = errors.New("remote client: start the resident owner before connecting this agent")

// managedEntryBlocked is conservative until the resident transaction owner
// is installed. An unreadable marker is not evidence of an unconfigured client.
func managedEntryBlocked(a *Agent) bool {
	if a.ID != "workbuddy" && a.ID != "hermes" {
		return false
	}
	for _, p := range []string{
		filepath.Join(appdir.Config(), "remote-connection.json"),
		filepath.Join(appdir.Config(), "remote-agent-sessions", a.ID+".json"),
	} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
}
