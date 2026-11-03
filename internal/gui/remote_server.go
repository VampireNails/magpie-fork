package gui

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/remotejoin"
	"github.com/yetone/magpie/internal/settings"
)

// remoteWebHandler is installed only on the protected Unix listener. All
// non-bootstrap paths retain the native one-port and page guards.
func remoteWebHandler(origin string, session webSession, resolver remotejoin.PeerResolver, page http.Handler) (http.Handler, error) {
	origin, err := remotejoin.RootOrigin(origin)
	if err != nil {
		return nil, err
	}
	if resolver == nil {
		return nil, errors.New("remote identity resolver is required")
	}
	registration, err := remotejoin.NewRegistrationHandler(remotejoin.ServerConfig{BaseURL: origin, ServerVersion: Version, Resolver: resolver, WebSessionReady: func(context.Context) error { return CheckWebKey(session.key) }})
	if err != nil {
		return nil, err
	}
	native := onePort(gateway.AllowGuard(webCompressed(session.guard(page))))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		// Tailscale Serve's Unix backend uses localhost as Host and replaces
		// X-Forwarded-Host with the incoming authority. This header is only a
		// value to compare, never identity; the resolver still requires the
		// protected connection and performs a local whois lookup.
		if host == "localhost" {
			hosts := r.Header.Values("X-Forwarded-Host")
			protos := r.Header.Values("X-Forwarded-Proto")
			if len(hosts) != 1 || len(protos) != 1 || protos[0] != "https" || strings.ContainsAny(hosts[0], "/\\?#") {
				http.Error(w, "invalid server host", http.StatusBadRequest)
				return
			}
			host = hosts[0]
		}
		got, err := remotejoin.RootOrigin("https://" + host)
		if err != nil || got != origin {
			http.Error(w, "invalid server host", http.StatusBadRequest)
			return
		}
		// Downstream same-origin guards must see the configured public URL,
		// not the Unix proxy's localhost and plaintext transport.
		r = r.Clone(r.Context())
		public, _ := url.Parse(origin)
		r.Host = public.Host
		r.URL.Scheme = public.Scheme
		if r.URL.Path != remotejoin.BrowserLoginPath && r.URL.Path != remotejoin.RegistrationPath {
			native.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.URL.Path == remotejoin.RegistrationPath {
			registration.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", 405)
			return
		}
		origins := r.Header.Values("Origin")
		if len(origins) > 1 {
			http.Error(w, "invalid origin", 400)
			return
		}
		if len(origins) == 1 {
			o, e := remotejoin.RootOrigin(origins[0])
			if e != nil || o != origin {
				http.Error(w, "invalid origin", 400)
				return
			}
		}
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(query) > 1 || (len(query) == 1 && (len(query["back"]) != 1)) {
			http.Error(w, "invalid return path", 400)
			return
		}
		back, err := remotejoin.ValidateReturnPath(query.Get("back"))
		if err != nil {
			http.Error(w, "invalid return path", 400)
			return
		}
		peer, err := resolver.Resolve(r.Context(), r)
		if err != nil {
			status := 503
			if errors.Is(err, remotejoin.ErrInvalidRequest) {
				status = 400
			}
			if errors.Is(err, remotejoin.ErrIdentityRequired) {
				status = 401
			}
			http.Error(w, "Tailscale identity unavailable", status)
			return
		}
		if peer.StableNodeID == "" || peer.NodeID == "" {
			http.Error(w, "Tailscale identity unavailable", 401)
			return
		}
		if CheckWebKey(session.key) != nil {
			http.Error(w, "web session unavailable", 503)
			return
		}
		session.signIn(w, r, back)
	}), nil
}

// StartRemoteWeb serves a complete native page over the protected Unix socket.
// Tailscale Serve must terminate HTTPS at publicURL and forward its root here.
func StartRemoteWeb(socket, publicURL, version string) (*Web, error) {
	host, portString, err := net.SplitHostPort(os.Getenv("MAGPIE_ADDR"))
	ip := net.ParseIP(host)
	n, portErr := strconv.Atoi(portString)
	if err != nil || ip == nil || !ip.IsLoopback() || portErr != nil || n < 1 || n > 65535 {
		return nil, errors.New("remote management requires MAGPIE_ADDR set to a loopback IP and port")
	}
	origin, err := remotejoin.RootOrigin(publicURL)
	if err != nil {
		return nil, err
	}
	u, _ := url.Parse(origin)
	port := u.Port()
	if port == "" {
		port = "443"
	}
	key := os.Getenv("MAGPIE_WEB_KEY")
	if key == "" {
		return nil, errors.New("remote management requires MAGPIE_WEB_KEY")
	}
	if err := CheckWebKey(key); err != nil {
		return nil, err
	}
	s, err := settings.LoadChecked()
	if err != nil {
		return nil, errors.New("cannot read settings for remote management")
	}
	if s.GatewayMode != "off" {
		return nil, errors.New("remote management requires gatewayMode=off to expose the complete management page")
	}
	if !s.LAN {
		return nil, errors.New("remote management requires gateway sharing enabled")
	}
	ln, err := listenProtectedUnix(socket)
	if err != nil {
		return nil, err
	}
	Version = version
	w := &Web{Addr: socket, Link: origin + "/", key: key, quit: make(chan struct{})}
	var once sync.Once
	quit := func() { once.Do(func() { close(w.quit) }) }
	session := webSession{cookie: "magpie_web_" + port, key: key, keep: webCookieAge, secure: true}
	var page http.Handler
	handler, err := remoteWebHandler(origin, session, remotejoin.NewUnixLocalAPIResolver("/var/run/tailscale/tailscaled.sock", 3*time.Second), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { page.ServeHTTP(w, r) }))
	if err != nil {
		ln.Close()
		return nil, err
	}
	page = Handler(webHost{quit}, startBackend())
	w.srv = &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second, ConnContext: remotejoin.TrustedServeConnContext}
	go func() { _ = w.srv.Serve(ln); quit() }()
	updates.autoRestart = func() bool {
		if restartInto(true, false, "", true) {
			quit()
			return true
		}
		return false
	}
	updates.start()
	news.start()
	return w, nil
}
