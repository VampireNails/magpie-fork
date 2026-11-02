package cli

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/gui"
	"github.com/yetone/magpie/internal/settings"
)

func webOutput(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old; w.Close() }()
	done := make(chan string, 1)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	f()
	w.Close()
	return <-done
}

func TestWebStartupOutputKeepsCredentialsPrivate(t *testing.T) {
	const key = "fake-web-admin-0123456789"
	for _, addr := range []string{"127.0.0.1:3430", "0.0.0.0:3430"} {
		t.Run(addr, func(t *testing.T) {
			t.Setenv("MAGPIE_WEB_KEY", key)
			t.Setenv("MAGPIE_PUBLIC_URL", "https://magpie.example.com")
			t.Setenv("MAGPIE_WEB_URL", "https://page.example.com/base")
			w := &gui.Web{Addr: addr, Link: "http://127.0.0.1:3430/?k=" + key}
			out := webOutput(t, func() { printWeb(w) })
			if strings.Contains(out, key) || strings.Contains(out, "?k=") {
				t.Error("ordinary startup output disclosed a credential or sign-in link")
			}
			if !strings.Contains(out, "http://127.0.0.1:3430/") || !strings.Contains(out, "MAGPIE_WEB_KEY") {
				t.Error("startup output lost the page address or headless sign-in guidance")
			}
			if !strings.Contains(w.Link, key) {
				t.Error("the private link used to open the browser lost its key")
			}
		})
	}
}

func TestWebExistingAppOutputKeepsCredentialsPrivate(t *testing.T) {
	helpHome(t)
	t.Setenv("MAGPIE_ADDR", "127.0.0.1:1")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	const key = "fake-app-admin-0123456789"
	port := ln.Addr().(*net.TCPAddr).Port
	if err := os.MkdirAll(settings.Dir(), 0700); err != nil {
		t.Fatal(err)
	}
	conf := fmt.Sprintf(`{"mode":"local","port":%d,"key":%q}`, port, key)
	if err := os.WriteFile(filepath.Join(settings.Dir(), "web-page.json"), []byte(conf), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := runOut(t, "web", "--no-open", "--addr", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, key) || strings.Contains(out, "?k=") || !strings.Contains(out, "the magpie app serves this page") {
		t.Error("reusing the app page disclosed credentials or did not find the page")
	}
}

func TestWebHeadlessNeedsKnownKey(t *testing.T) {
	helpHome(t)
	t.Setenv("MAGPIE_WEB_KEY", "")
	t.Setenv("MAGPIE_ADDR", "127.0.0.1:1")
	// An invalid address also keeps the old implementation from starting a
	// server: the error must explain the missing usable sign-in credential.
	for _, args := range [][]string{{"web", "--no-open", "--addr", "invalid"}, {"serve", "--web=127.0.0.1:invalid"}} {
		_, err := runOut(t, args...)
		if err == nil || !strings.Contains(err.Error(), "MAGPIE_WEB_KEY") {
			t.Errorf("%s did not explain how to sign in without startup credentials", args[0])
		}
	}
}

func TestDaemonPageOutputKeepsCredentialsPrivate(t *testing.T) {
	const key = "fake-daemon-admin-0123456789"
	t.Setenv("MAGPIE_PUBLIC_URL", "https://magpie.example.com")
	for _, addr := range []string{"127.0.0.1:3430", "0.0.0.0:3430"} {
		out := webOutput(t, func() { printDaemonWeb(addr, key) })
		if strings.Contains(out, key) || strings.Contains(out, "?k=") {
			t.Error("ordinary service status or install output disclosed the sign-in link")
		}
		if !strings.Contains(out, "http://127.0.0.1:3430/") || !strings.Contains(out, "magpie daemon web") {
			t.Error("service output lost its address or explicit credential-view command")
		}
		private := webOutput(t, func() { printDaemonPage(addr, key, true) })
		if !strings.Contains(private, "/?k="+key) {
			t.Error("the explicit credential view lost the usable sign-in link")
		}
	}
}

// magpie web on loopback behind a reverse proxy (#1479) prints the proxy's
// link with the key when told the proxy's address, so nobody joins
// "/?k=…" onto it by hand: MAGPIE_WEB_URL, else MAGPIE_PUBLIC_URL with no
// port (one with a port isn't a proxy). None is printed without either.
func TestWebProxyLink(t *testing.T) {
	key := "/?k=0123456789abcdef"
	for _, c := range []struct{ env, want string }{
		{"https://magpie.example.com", "https://magpie.example.com/?k=0123456789abcdef"},
		{"https://magpie.example.com/", "https://magpie.example.com/?k=0123456789abcdef"},
		{"magpie.example.com", "http://magpie.example.com/?k=0123456789abcdef"},
		{"https://example.com/magpie/", "https://example.com/magpie/?k=0123456789abcdef"},
		{"http://192.168.1.20:3425", ""},
		{"", ""},
	} {
		t.Setenv("MAGPIE_PUBLIC_URL", c.env)
		if got := proxyLink(key); got != c.want {
			t.Errorf("MAGPIE_PUBLIC_URL=%q: %q, want %q", c.env, got, c.want)
		}
	}
	// the page's own proxy address, when the gateway's is elsewhere
	t.Setenv("MAGPIE_PUBLIC_URL", "https://api.example.com")
	for _, c := range []struct{ env, want string }{
		{"https://magpie.example.com/", "https://magpie.example.com/?k=0123456789abcdef"},
		{"magpie.example.com:8443", "https://magpie.example.com:8443/?k=0123456789abcdef"},
		{"https://magpie.example.com/?x=1", ""},
		{"ftp://magpie.example.com", ""},
	} {
		t.Setenv("MAGPIE_WEB_URL", c.env)
		if got := proxyLink(key); got != c.want {
			t.Errorf("MAGPIE_WEB_URL=%q: %q, want %q", c.env, got, c.want)
		}
	}
}

// `magpie web` opens the app's page when the app serves it on the port it
// would take, and starts its own on any other.
func TestWebSamePort(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"127.0.0.1:3430", "127.0.0.1:3430", true},
		{"0.0.0.0:3430", "127.0.0.1:3430", true},
		{"127.0.0.1:3430", "0.0.0.0:3431", false},
		{"", "127.0.0.1:3430", false},
		{"127.0.0.1:3430", "3430", false},
	} {
		if got := samePort(c.a, c.b); got != c.want {
			t.Errorf("samePort(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
