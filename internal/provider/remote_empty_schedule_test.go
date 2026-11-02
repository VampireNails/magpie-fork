package provider

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/catalog"
)

func TestRemoteScheduledRefreshEmptyAndInvalid(t *testing.T) {
	autolistHome(t)
	relay := newRelayList(t, "one", "two")
	if err := Save(Provider{ID: "office", Name: "Office", Preset: RemoteMagpiePreset, Chat: relay.srv.URL, Key: "fake-key"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listsTried.Lock(); clear(listsTried.m); listsTried.Unlock() })
	now := time.Now()
	if got := refreshDueLists(context.Background(), now); !slices.Equal(got, []string{"office"}) {
		t.Fatalf("initial scheduled refresh skipped: %v", got)
	}
	before, at, ok := catalog.Live("office")
	if !ok || len(before) != 2 {
		t.Fatalf("initial catalog: %v %v", before, ok)
	}
	relay.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"data":null}`)) })
	if got := refreshDueLists(context.Background(), now.Add(11*time.Minute)); !slices.Equal(got, []string{"office"}) {
		t.Fatalf("invalid scheduled refresh skipped: %v", got)
	}
	after, at2, ok := catalog.Live("office")
	if !ok || len(after) != 2 || !at.Equal(at2) {
		t.Fatalf("invalid list changed catalog: %v %v %v", after, at2, ok)
	}
	relay.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"data":[]}`)) })
	if got := refreshDueLists(context.Background(), now.Add(22*time.Minute)); !slices.Equal(got, []string{"office"}) {
		t.Fatalf("empty scheduled refresh skipped: %v", got)
	}
	after, _, ok = catalog.Live("office")
	if !ok || len(after) != 0 {
		t.Fatalf("empty scheduled list not stored: %v %v", after, ok)
	}
}
