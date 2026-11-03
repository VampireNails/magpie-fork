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
