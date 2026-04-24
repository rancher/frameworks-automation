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

func TestFindGoModDirs(t *testing.T) {
	root := t.TempDir()

	write := func(rel string, content string) {
		t.Helper()
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("go.mod", "module root\ngo 1.21\n")
	write("subpkg/go.mod", "module sub\ngo 1.21\n")
	// vendor/ must be skipped entirely
	write("vendor/github.com/foo/go.mod", "module vendored\ngo 1.21\n")

	dirs, err := findGoModDirs(root)
	if err != nil {
		t.Fatalf("findGoModDirs: %v", err)
	}
	if len(dirs) != 2 {
		t.Errorf("want 2 dirs (root + subpkg), got %d: %v", len(dirs), dirs)
	}
}

func TestGoModContains(t *testing.T) {
	dir := t.TempDir()
	gomod := filepath.Join(dir, "go.mod")
	content := "module root\ngo 1.21\nrequire github.com/rancher/steve v0.7.4\n"
	if err := os.WriteFile(gomod, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	has, err := goModContains(gomod, "github.com/rancher/steve")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !has {
		t.Error("want true for present module")
	}

	// longer module name sharing a prefix must not match
	has, err = goModContains(gomod, "github.com/rancher/steve-extra")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if has {
		t.Error("want false for module with same prefix but different name")
	}

	has, err = goModContains(gomod, "github.com/rancher/other")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if has {
		t.Error("want false for absent module")
	}
}
