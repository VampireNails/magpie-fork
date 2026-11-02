package provider

import "testing"

func TestRemoteBindingStaleEditCannotLoseAnotherChange(t *testing.T) {
	azureHome(t)
	stale, err := read()
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(Provider{ID: "other", Chat: "https://other.invalid/v1", Key: "other-key"}); err != nil {
		t.Fatal(err)
	}
	stale.Providers = append(stale.Providers, Provider{ID: "remote-fixed"})
	if err := store(stale); err == nil {
		t.Fatal("stale edit overwrote concurrent provider change")
	}
	if _, err := Find("other"); err != nil {
		t.Fatal("other provider disappeared")
	}
}
