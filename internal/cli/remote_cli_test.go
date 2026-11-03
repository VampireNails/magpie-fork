package cli

import (
	"strings"
	"testing"
)

func TestRemoteStatusRequiresOwner(t *testing.T) {
	cliHome(t)
	err := run([]string{"remote", "status", "--json"})
	if err == nil || !strings.Contains(err.Error(), "remote run") {
		t.Fatalf("remote status must explain the missing owner, got %v", err)
	}
}
