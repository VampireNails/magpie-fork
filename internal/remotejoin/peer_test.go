package remotejoin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/testenv"
)

func TestMain(m *testing.M) { testenv.Main(m) }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func localFixture(t *testing.T, status int, body string) *LocalAPIResolver {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/localapi/v0/whois" || r.URL.Query().Get("addr") != "100.75.211.11" {
			t.Error("unexpected whois request")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		copy.URL.Scheme = "http"
		copy.URL.Host = s.Listener.Addr().String()
		return s.Client().Transport.RoundTrip(copy)
	})}
	return NewLocalAPIResolver(c, time.Second)
}

func peerRequest(trusted bool) *http.Request {
	r := httptest.NewRequest("POST", "https://node.test/", nil)
	r.Header.Set("X-Forwarded-For", "100.75.211.11")
	r.Header.Set("X-Magpie-Node-ID", "forged")
	r.Header.Set("Tailscale-User-Login", "forged")
	if trusted {
		r = r.WithContext(TrustedServeConnContext(r.Context(), nil))
	}
	return r
}

func TestPeerTrustedServeUsesWhoisWithoutUserHeader(t *testing.T) {
	r := localFixture(t, 200, `{"Node":{"StableID":"node-one","ID":9007199254740993,"ComputedName":"same-name","Tags":["tag:server"]}}`)
	req := peerRequest(true)
	req.Header.Del("Tailscale-User-Login")
	p, err := r.Resolve(req.Context(), req)
	if err != nil || p.StableNodeID != "node-one" || p.NodeID != "9007199254740993" || p.DisplayName != "same-name" {
		t.Fatal("trusted whois node was not resolved exactly")
	}
}

func TestPeerTCPHeadersCannotCreateTrustedIdentity(t *testing.T) {
	r := localFixture(t, 200, `{"Node":{"StableID":"node-one","ID":1,"ComputedName":"node"}}`)
	req := peerRequest(false)
	_, err := r.Resolve(req.Context(), req)
	if !errors.Is(err, ErrIdentityRequired) {
		t.Fatal("ordinary TCP spoof was accepted")
	}
}

func TestPeerRejectsAmbiguousForwardedSource(t *testing.T) {
	for _, values := range [][]string{{"100.75.211.11,100.69.98.89"}, {"100.75.211.11", "100.69.98.89"}, {"unknown"}} {
		r := localFixture(t, 200, `{}`)
		req := peerRequest(true)
		req.Header["X-Forwarded-For"] = values
		_, err := r.Resolve(req.Context(), req)
		if !errors.Is(err, ErrInvalidRequest) {
			t.Error("ambiguous source needs invalid request")
		}
	}
}

func TestPeerLocalAPIFailureIsNotMissingIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"not-found", 404, "private-canary", ErrIdentityRequired},
		{"no-node", 200, `{"Node":null}`, ErrIdentityRequired},
		{"incomplete", 200, `{"Node":{"ID":1,"ComputedName":"node"}}`, ErrIdentityRequired},
		{"unavailable", 500, "private-canary", ErrIdentityUnavailable},
		{"bad-json", 200, "private-canary", ErrIdentityUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := localFixture(t, tc.status, tc.body)
			req := peerRequest(true)
			_, err := r.Resolve(req.Context(), req)
			if !errors.Is(err, tc.want) || err.Error() == tc.body {
				t.Fatal("incorrect or unsafe identity error")
			}
		})
	}
}

func TestPeerLocalAPITimeout(t *testing.T) {
	c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })}
	r := NewLocalAPIResolver(c, time.Millisecond)
	req := peerRequest(true)
	_, err := r.Resolve(context.Background(), req)
	if !errors.Is(err, ErrIdentityUnavailable) {
		t.Fatal("timeout must be retryable identity lookup failure")
	}
}
