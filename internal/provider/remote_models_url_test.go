package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
)

func TestRemoteModelsURLTakesPrecedence(t *testing.T) {
	autolistHome(t)
	var baseHits, listHits atomic.Int32
	base := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		baseHits.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"wrong"}]}`))
	}))
	defer base.Close()
	var body atomic.Value
	body.Store(`{"data":[{"id":"one"}]}`)
	list := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		listHits.Add(1)
		if req.URL.Path != "/custom" {
			t.Errorf("unexpected path %s", req.URL.Path)
		}
		_, _ = w.Write([]byte(body.Load().(string)))
	}))
	defer list.Close()
	if err := Save(Provider{ID: "office", Name: "Office", Preset: RemoteMagpiePreset, Chat: base.URL, ModelsURL: list.URL + "/custom", Key: "fake-key"}); err != nil {
		t.Fatal(err)
	}
	p, err := Find("office")
	if err != nil {
		t.Fatal(err)
	}
	for _, quiet := range []bool{false, true} {
		refresh := func() error {
			if quiet {
				return p.RefetchRemoteQuiet(context.Background())
			}
			_, _, err := p.Refetch(context.Background())
			return err
		}
		body.Store(`{"data":[{"id":"one"}]}`)
		if err := refresh(); err != nil {
			t.Fatal(err)
		}
		old, at, ok := catalog.Live("office")
		if !ok || len(old) != 1 || old[0].ID != "one" {
			t.Fatalf("initial list %v %v", old, ok)
		}
		body.Store(`{"data":[{"name":"bad"}]}`)
		if err := refresh(); err == nil {
			t.Fatal("invalid explicit list accepted")
		}
		after, at2, ok := catalog.Live("office")
		if !ok || len(after) != 1 || after[0].ID != "one" || !at.Equal(at2) {
			t.Fatalf("invalid list changed old data: %v", after)
		}
		body.Store(`{"data":[]}`)
		if err := refresh(); err != nil {
			t.Fatal(err)
		}
		after, _, ok = catalog.Live("office")
		if !ok || len(after) != 0 {
			t.Fatalf("empty list not saved: %v %v", after, ok)
		}
	}
	if baseHits.Load() != 0 || listHits.Load() != 6 {
		t.Fatalf("wrong endpoint: base=%d list=%d", baseHits.Load(), listHits.Load())
	}
}
