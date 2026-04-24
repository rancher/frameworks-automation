package pr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rancher/release-automation/internal/config"
)

func TestLookupStrategy(t *testing.T) {
	for _, s := range []config.Strategy{config.StrategyGoGet, config.StrategyChartBump, config.StrategyBumpWebhook} {
		if _, err := lookupStrategy(s); err != nil {
			t.Errorf("strategy %q should be registered, got %v", s, err)
		}
	}
}

func TestLookupStrategy_OrderRejected(t *testing.T) {
	// Order is sequencing-only — the cascade filters it out before it
	// reaches the bumper. If one ever leaks through, lookup must fail loudly
	// rather than silently no-op the bump.
	if _, err := lookupStrategy(config.StrategyOrder); err == nil {
		t.Fatal("StrategyOrder must NOT be in the bumper registry")
	}
}

func TestLookupStrategy_UnknownErrors(t *testing.T) {
	if _, err := lookupStrategy("not-a-strategy"); err == nil {
		t.Fatal("expected error for unknown strategy")
	}
}

func TestScriptStrategy_RunsBodyAgainstRepoDir(t *testing.T) {
	dir := t.TempDir()
	body := "#!/usr/bin/env bash\nset -e\necho \"$1\" > marker\n"
	s := scriptStrategy{name: "test", body: body}
	if err := s.Apply(context.Background(), dir, Module{Version: "v9.9.9"}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "marker"))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if strings.TrimSpace(string(got)) != "v9.9.9" {
		t.Errorf("marker: got %q want v9.9.9", got)
	}
}

func TestScriptStrategy_EmptyBodyErrors(t *testing.T) {
	s := scriptStrategy{name: "empty"}
	if err := s.Apply(context.Background(), t.TempDir(), Module{Version: "v1"}); err == nil {
		t.Fatal("expected error for empty body")
	}
}
