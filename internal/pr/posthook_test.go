package pr

import (
	"testing"

	"github.com/rancher/release-automation/internal/config"
)

func TestLookupPostBundleHook(t *testing.T) {
	for _, h := range []config.PostBundleHook{
		config.PostBundleSyncDeps,
	} {
		if _, err := lookupPostBundleHook(h); err != nil {
			t.Errorf("hook %q should be registered, got %v", h, err)
		}
	}
}

func TestLookupPostBundleHook_UnknownErrors(t *testing.T) {
	if _, err := lookupPostBundleHook("not-a-hook"); err == nil {
		t.Fatal("expected error for unknown hook")
	}
}
