package gui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type remoteFixture struct {
	err error
	url string
}

func (f remoteFixture) Status(context.Context) (remoteClientStatus, error) {
	return remoteClientStatus{State: "unconfigured"}, f.err
}
func (f remoteFixture) Connect(context.Context, string) (remoteClientStatus, error) {
	return f.Status(context.Background())
}
func (f remoteFixture) Refresh(context.Context) (remoteClientStatus, error) {
	return f.Status(context.Background())
}
func (f remoteFixture) Disconnect(context.Context) (remoteClientStatus, error) {
	return f.Status(context.Background())
}
func (f remoteFixture) ManagementURL(context.Context) (string, error) { return f.url, f.err }
func remoteClientTestHandler() http.Handler {
	return localRemoteHandler(Handler(nil, nil), remoteFixture{})
}
func TestRemoteClientLocalStatusIsAvailable(t *testing.T) {
	w := httptest.NewRecorder()
	remoteClientTestHandler().ServeHTTP(w, httptest.NewRequest("GET", "/api/remote-connection", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"unconfigured"`) {
		t.Fatalf("status got %d", w.Code)
	}
}
func TestRemoteClientServerDoesNotInstallLocalAPI(t *testing.T) {
	w := httptest.NewRecorder()
	Handler(nil, nil).ServeHTTP(w, httptest.NewRequest("GET", "/api/remote-connection", nil))
	if w.Code != 404 {
		t.Fatalf("server status %d", w.Code)
	}
	w = httptest.NewRecorder()
	Handler(nil, nil).ServeHTTP(w, httptest.NewRequest("GET", "/api/notices", nil))
	if w.Code != 200 {
		t.Fatalf("native status %d", w.Code)
	}
}
func TestRemoteClientRejectsInvalidBodyAndSecrets(t *testing.T) {
	h := remoteClientTestHandler()
	for _, body := range []string{`{"unknown":1}`, `{} {}`, strings.Repeat(" ", 4097) + `{}`} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/api/remote-connection", strings.NewReader(body)))
		if w.Code != 400 && w.Code != 413 {
			t.Errorf("invalid body accepted: %d", w.Code)
		}
	}
	h = localRemoteHandler(http.NotFoundHandler(), remoteFixture{err: errors.New("SECRET_CANARY")})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/remote-connection", nil))
	if w.Code != 503 || strings.Contains(w.Body.String(), "SECRET_CANARY") {
		t.Fatal("unsafe error")
	}
	h = localRemoteHandler(http.NotFoundHandler(), remoteFixture{url: "https://host/_magpie/remote/v1/browser-login?k=SECRET_CANARY"})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/api/remote-connection/manage", strings.NewReader(`{}`)))
	if w.Code != 503 || strings.Contains(w.Body.String(), "SECRET_CANARY") {
		t.Fatal("unsafe URL")
	}
}
func TestRemoteClientRejectsFramedRequests(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/remote-connection", strings.NewReader(`{}`))
	r.Header.Set("Origin", "null")
	w := httptest.NewRecorder()
	remoteClientTestHandler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("framed request: %d", w.Code)
	}
}

func TestRemoteClientRejectsForeignOriginWithoutFetchMetadata(t *testing.T) {
	r := httptest.NewRequest("POST", "http://localhost/api/remote-connection", strings.NewReader(`{}`))
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	remoteClientTestHandler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("foreign origin accepted: %d", w.Code)
	}
}

func TestRemoteClientRejectsDifferentScheme(t *testing.T) {
	r := httptest.NewRequest("POST", "http://localhost/api/remote-connection", strings.NewReader(`{}`))
	r.Header.Set("Origin", "https://localhost")
	w := httptest.NewRecorder()
	remoteClientTestHandler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("foreign scheme accepted: %d", w.Code)
	}
}

type countingRemoteFixture struct{ calls int }

func (f *countingRemoteFixture) Status(context.Context) (remoteClientStatus, error) {
	f.calls++
	return remoteClientStatus{State: "unconfigured"}, nil
}
func (f *countingRemoteFixture) Connect(context.Context, string) (remoteClientStatus, error) {
	f.calls++
	return remoteClientStatus{State: "unconfigured"}, nil
}
func (f *countingRemoteFixture) Refresh(context.Context) (remoteClientStatus, error) {
	f.calls++
	return remoteClientStatus{State: "unconfigured"}, nil
}
func (f *countingRemoteFixture) Disconnect(context.Context) (remoteClientStatus, error) {
	f.calls++
	return remoteClientStatus{State: "paused"}, nil
}
func (f *countingRemoteFixture) ManagementURL(context.Context) (string, error) {
	f.calls++
	return "", nil
}
func TestRemoteClientRejectsNonObjectWithoutServiceCall(t *testing.T) {
	for _, path := range []string{"/api/remote-connection", "/api/remote-connection/refresh", "/api/remote-connection/disconnect", "/api/remote-connection/manage"} {
		for _, body := range []string{"null", "[]", "true", "42", `"x"`} {
			f := &countingRemoteFixture{}
			h := localRemoteHandler(http.NotFoundHandler(), f)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(body)))
			if w.Code != 400 || f.calls != 0 {
				t.Errorf("%s %s: status %d, calls %d", path, body, w.Code, f.calls)
			}
		}
	}
}
func TestRemoteClientEmptyObjectCallsConnect(t *testing.T) {
	f := &countingRemoteFixture{}
	w := httptest.NewRecorder()
	localRemoteHandler(http.NotFoundHandler(), f).ServeHTTP(w, httptest.NewRequest("POST", "/api/remote-connection", strings.NewReader(`{}`)))
	if w.Code != 200 || f.calls != 1 {
		t.Fatalf("status %d calls %d", w.Code, f.calls)
	}
}

func TestRemoteClientBootFlagOnlyForDesktop(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.Handler
		want bool
	}{{"desktop", desktopRemoteHandler(nil, nil, remoteFixture{}), true}, {"server", Handler(nil, nil), false}} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tc.h.ServeHTTP(w, httptest.NewRequest("GET", "/boot.js", nil))
			got := strings.Contains(w.Body.String(), `"remoteClient":true`)
			if w.Code != 200 || got != tc.want {
				t.Fatalf("boot flag got %v, status %d", got, w.Code)
			}
		})
	}
}
func TestRemoteClientRejectsNullServerURLWithoutServiceCall(t *testing.T) {
	for _, body := range []string{`{"serverUrl":null}`, `{"serverUrl":42}`, `{"serverUrl":false}`, `{"serverUrl":[]}`} {
		f := &countingRemoteFixture{}
		w := httptest.NewRecorder()
		localRemoteHandler(http.NotFoundHandler(), f).ServeHTTP(w, httptest.NewRequest("POST", "/api/remote-connection", strings.NewReader(body)))
		if w.Code != 400 || f.calls != 0 {
			t.Errorf("%s: status %d calls %d", body, w.Code, f.calls)
		}
	}
}
func TestRemoteClientAcceptsEmptyServerURL(t *testing.T) {
	for _, body := range []string{`{}`, `{"serverUrl":""}`} {
		f := &countingRemoteFixture{}
		w := httptest.NewRecorder()
		localRemoteHandler(http.NotFoundHandler(), f).ServeHTTP(w, httptest.NewRequest("POST", "/api/remote-connection", strings.NewReader(body)))
		if w.Code != 200 || f.calls != 1 {
			t.Errorf("%s: status %d calls %d", body, w.Code, f.calls)
		}
	}
}

func TestRemoteClientDesktopTransportRequired(t *testing.T) {
	f := &countingRemoteFixture{}
	h := desktopRemoteHandler(nil, nil, f)
	r := httptest.NewRequest("POST", "http://localhost/api/remote-connection", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 || f.calls != 0 {
		t.Fatalf("untrusted transport status %d calls %d", w.Code, f.calls)
	}
	r = httptest.NewRequest("POST", "http://localhost/api/remote-connection", strings.NewReader(`{}`))
	w = httptest.NewRecorder()
	ownWindow(h).ServeHTTP(w, r)
	if w.Code != 200 || f.calls != 1 {
		t.Fatalf("desktop transport status %d calls %d", w.Code, f.calls)
	}
}
