package cli

import (
	"strings"
	"testing"
)

func TestServeRemoteRequiresBothSocketAndURL(t *testing.T) {
	err := serveCmd([]string{"--remote-socket=/tmp/magpie/serve.sock"})
	if err == nil || !strings.Contains(err.Error(), "--remote-url") {
		t.Fatal("remote socket requires an explicit public URL")
	}
}
