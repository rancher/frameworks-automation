package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeYAML drops `body` into a temp file and returns the path. Used so the
// tests exercise the real Load() path (parse + applyDefaults + validate).
func writeYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deps.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestLoad_ObjectDepsAndDefaultStrategy(t *testing.T) {
	path := writeYAML(t, `
repos:
  rancher:
    kind: leaf
    repo: x/rancher
    deps:
      - {name: steve}
      - {name: webhook, strategy: bump-webhook}
  steve:
    kind: paired
    repo: x/steve
  webhook:
    kind: paired
    repo: x/webhook
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	rancher := cfg.Repos["rancher"]
	if len(rancher.Deps) != 2 {
		t.Fatalf("rancher deps: %+v", rancher.Deps)
	}
	if rancher.Deps[0].Name != "steve" || rancher.Deps[0].Strategy != StrategyGoGet {
		t.Errorf("steve dep should default to go-get: %+v", rancher.Deps[0])
	}
	if rancher.Deps[1].Name != "webhook" || rancher.Deps[1].Strategy != StrategyBumpWebhook {
		t.Errorf("webhook dep should preserve strategy: %+v", rancher.Deps[1])
	}
}

func TestLoad_BranchTemplate(t *testing.T) {
	path := writeYAML(t, `
repos:
  rancher:
    kind: leaf
    repo: x/rancher
    deps:
      - {name: chart, strategy: order}
  chart:
    kind: paired
    repo: x/chart
    branch-template: "dev-v{rancher-minor}"
    deps: []
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	chart := cfg.Repos["chart"]
	if chart.BranchTemplate != "dev-v{rancher-minor}" {
		t.Errorf("branch-template: got %q", chart.BranchTemplate)
	}
}

func TestLoad_ForkField(t *testing.T) {
	path := writeYAML(t, `
repos:
  rancher:
    kind: leaf
    repo: x/rancher
    deps:
      - {name: wrangler}
  wrangler:
    kind: independent
    repo: x/wrangler
    fork: bot/wrangler
    deps: []
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Repos["wrangler"].Fork != "bot/wrangler" {
		t.Errorf("fork: got %q", cfg.Repos["wrangler"].Fork)
	}
}

func TestLoad_RejectsMissingRepo(t *testing.T) {
	path := writeYAML(t, `
repos:
  rancher:
    kind: leaf
    deps: []
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "repo is required") {
		t.Errorf("want 'repo is required' error, got %v", err)
	}
}

func TestLoad_RejectsMalformedRepo(t *testing.T) {
	path := writeYAML(t, `
repos:
  rancher:
    kind: leaf
    repo: "not-owner-slash-name"
    deps: []
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "must be owner/name") {
		t.Errorf("want 'must be owner/name' error, got %v", err)
	}
}

func TestLoad_RejectsMalformedFork(t *testing.T) {
	path := writeYAML(t, `
repos:
  rancher:
    kind: leaf
    repo: x/rancher
    deps:
      - {name: wrangler}
  wrangler:
    kind: independent
    repo: x/wrangler
    fork: "not-valid"
    deps: []
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "must be owner/name") {
		t.Errorf("want 'must be owner/name' error, got %v", err)
	}
}

func TestLoad_RejectsMissingDepName(t *testing.T) {
	path := writeYAML(t, `
repos:
  rancher:
    kind: leaf
    repo: x/rancher
    deps:
      - {strategy: go-get}
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "missing name") {
		t.Errorf("want 'missing name' error, got %v", err)
	}
}

func TestLoad_RejectsUnknownStrategy(t *testing.T) {
	path := writeYAML(t, `
repos:
  rancher:
    kind: leaf
    repo: x/rancher
    deps:
      - {name: steve, strategy: nope}
  steve:
    kind: paired
    repo: x/steve
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unknown strategy") {
		t.Errorf("want 'unknown strategy' error, got %v", err)
	}
}

func TestLoad_RejectsDuplicateDep(t *testing.T) {
	path := writeYAML(t, `
repos:
  rancher:
    kind: leaf
    repo: x/rancher
    deps:
      - {name: steve}
      - {name: steve, strategy: bump-webhook}
  steve:
    kind: paired
    repo: x/steve
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "listed twice") {
		t.Errorf("want 'listed twice' error, got %v", err)
	}
}

func TestLoad_NextTagStrategyDefaults(t *testing.T) {
	path := writeYAML(t, `
repos:
  rancher:
    kind: leaf
    repo: x/rancher
    deps:
      - {name: webhook, strategy: bump-webhook}
  webhook:
    kind: paired
    repo: x/webhook
    next-tag-strategy: rc
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Repos["rancher"].NextTagStrategy != NextTagPatch {
		t.Errorf("rancher next-tag-strategy should default to patch, got %q", cfg.Repos["rancher"].NextTagStrategy)
	}
	if cfg.Repos["webhook"].NextTagStrategy != NextTagRC {
		t.Errorf("webhook next-tag-strategy: got %q want %q", cfg.Repos["webhook"].NextTagStrategy, NextTagRC)
	}
}

func TestLoad_AcceptsUnRCNextTagStrategy(t *testing.T) {
	path := writeYAML(t, `
repos:
  rancher:
    kind: leaf
    repo: x/rancher
    deps:
      - {name: webhook, strategy: bump-webhook}
  webhook:
    kind: paired
    repo: x/webhook
    next-tag-strategy: unrc
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Repos["webhook"].NextTagStrategy != NextTagUnRC {
		t.Errorf("webhook next-tag-strategy: got %q want %q", cfg.Repos["webhook"].NextTagStrategy, NextTagUnRC)
	}
}

func TestLoad_RejectsUnknownNextTagStrategy(t *testing.T) {
	path := writeYAML(t, `
repos:
  rancher:
    kind: leaf
    repo: x/rancher
    next-tag-strategy: nope
    deps: []
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unknown next-tag-strategy") {
		t.Errorf("want unknown-next-tag-strategy error, got %v", err)
	}
}

func TestLoad_RejectsBranchTemplateOnNonPaired(t *testing.T) {
	path := writeYAML(t, `
repos:
  thing:
    kind: independent
    repo: x/thing
    branch-template: "dev-v{rancher-minor}"
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "branch-template is only valid on kind=paired") {
		t.Errorf("want branch-template/kind error, got %v", err)
	}
}

func TestLoad_RejectsUnknownPlaceholder(t *testing.T) {
	path := writeYAML(t, `
repos:
  chart:
    kind: paired
    repo: x/chart
    branch-template: "dev-v{leaf-branch}"
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unknown placeholder") {
		t.Errorf("want unknown-placeholder error, got %v", err)
	}
}

func TestResolveBranch_BranchTemplate(t *testing.T) {
	// {rancher-minor} expands to a value that already includes the "v"
	// prefix (e.g. "v2.16" via VERSION.md.LookupMinor). Templates therefore
	// shouldn't repeat the "v" — "dev-{rancher-minor}" yields "dev-v2.16".
	r := Repo{Kind: KindPaired, Repo: "x/chart", BranchTemplate: "dev-{rancher-minor}"}
	got, err := r.ResolveBranch("v2.16", nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "dev-v2.16" {
		t.Errorf("got %q want %q", got, "dev-v2.16")
	}
}

func TestResolveBranch_VersionTable(t *testing.T) {
	r := Repo{Kind: KindPaired, Repo: "x/steve"}
	tbl := &VersionTable{Rows: []VersionRow{
		{Branch: "release/v0.7", Minor: "v0.7", Pair: "v2.13"},
	}}
	got, err := r.ResolveBranch("v2.13", tbl)
	if err != nil || got != "release/v0.7" {
		t.Errorf("got %q err=%v", got, err)
	}
}

func TestResolveBranch_NoMatchReturnsEmpty(t *testing.T) {
	r := Repo{Kind: KindPaired, Repo: "x/steve"}
	tbl := &VersionTable{Rows: []VersionRow{{Branch: "main", Minor: "v0.9", Pair: "v2.15"}}}
	got, err := r.ResolveBranch("v2.13", tbl)
	if err != nil || got != "" {
		t.Errorf("expected empty string + nil err for no-match, got %q err=%v", got, err)
	}
}

func TestResolveBranch_MissingTableErrors(t *testing.T) {
	r := Repo{Kind: KindPaired, Repo: "x/steve"}
	if _, err := r.ResolveBranch("v2.13", nil); err == nil {
		t.Fatal("expected error for nil table on VERSION.md path")
	}
}

func TestDepStrategy(t *testing.T) {
	r := Repo{Deps: []Dep{
		{Name: "steve", Strategy: StrategyGoGet},
		{Name: "webhook", Strategy: StrategyBumpWebhook},
	}}
	if r.DepStrategy("webhook") != StrategyBumpWebhook {
		t.Errorf("webhook strategy: got %q", r.DepStrategy("webhook"))
	}
	if r.DepStrategy("missing") != StrategyGoGet {
		t.Errorf("missing dep should default to go-get, got %q", r.DepStrategy("missing"))
	}
}

func TestDependents_FindsAllRegardlessOfStrategy(t *testing.T) {
	cfg := &Config{Repos: map[string]Repo{
		"rancher": {Kind: KindLeaf, Repo: "x/rancher", Deps: []Dep{
			{Name: "chart", Strategy: StrategyOrder},
			{Name: "webhook", Strategy: StrategyBumpWebhook},
		}},
		"chart":   {Kind: KindPaired, Repo: "x/chart", BranchTemplate: "dev-v{rancher-minor}"},
		"webhook": {Kind: KindPaired, Repo: "x/webhook"},
	}}
	got := cfg.Dependents("chart")
	if len(got) != 1 || got[0] != "rancher" {
		t.Errorf("Dependents should include order-edge dependents (cascade walker needs them): got %v", got)
	}
}

func TestGitHubRepo(t *testing.T) {
	r := Repo{Repo: "rancher/wrangler"}
	got, err := r.GitHubRepo()
	if err != nil || got != "rancher/wrangler" {
		t.Errorf("got %q err=%v", got, err)
	}
}

func TestGitHubRepo_Empty(t *testing.T) {
	r := Repo{}
	if _, err := r.GitHubRepo(); err == nil {
		t.Fatal("expected error for empty Repo field")
	}
}

func TestResolveDep(t *testing.T) {
	cfg := &Config{Repos: map[string]Repo{
		"wrangler": {Kind: KindIndependent, Repo: "rancher/wrangler"},
	}}
	got, err := cfg.ResolveDep("rancher/wrangler")
	if err != nil || got != "wrangler" {
		t.Errorf("got %q err=%v", got, err)
	}
	if _, err := cfg.ResolveDep("rancher/unknown"); err == nil {
		t.Error("expected error for unknown repo")
	}
}

func TestModuleToRepo(t *testing.T) {
	cfg := &Config{
		Repos: map[string]Repo{
			"wrangler": {Repo: "rancher/wrangler"},
		},
		Modules: map[string][]string{
			"wrangler": {"github.com/rancher/wrangler", "github.com/rancher/wrangler/v3"},
		},
	}
	m := cfg.ModuleToRepo()
	if m["github.com/rancher/wrangler"] != "wrangler" {
		t.Errorf("expected wrangler for module, got %q", m["github.com/rancher/wrangler"])
	}
	if m["github.com/rancher/wrangler/v3"] != "wrangler" {
		t.Errorf("expected wrangler for v3 module, got %q", m["github.com/rancher/wrangler/v3"])
	}
}

func TestLoadAll_TwoConfigsKeyedByBasename(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rancher-chart-webhook.yaml"), []byte(`
repos:
  rancher:
    kind: leaf
    repo: x/rancher
    deps:
      - {name: webhook, strategy: bump-webhook}
  webhook:
    kind: paired
    repo: x/webhook
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rancher-chart-remotedialer-proxy.yaml"), []byte(`
repos:
  rancher:
    kind: leaf
    repo: x/rancher
    deps:
      - {name: remotedialer-proxy, strategy: bump-remotedialer-proxy}
  remotedialer-proxy:
    kind: paired
    repo: x/remotedialer-proxy
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgs, err := LoadAll(dir)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(cfgs) != 2 {
		t.Fatalf("want 2 configs, got %d: %v", len(cfgs), cfgs)
	}
	if _, ok := cfgs["rancher-chart-webhook"]; !ok {
		t.Errorf("missing rancher-chart-webhook config")
	}
	if _, ok := cfgs["rancher-chart-remotedialer-proxy"]; !ok {
		t.Errorf("missing rancher-chart-remotedialer-proxy config")
	}
}

func TestLoadAll_SkipsNonYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(`
repos:
  rancher:
    kind: leaf
    repo: x/rancher
    deps: []
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("ignore me"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgs, err := LoadAll(dir)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(cfgs) != 1 {
		t.Fatalf("want 1 config, got %d", len(cfgs))
	}
}

func TestLoadAll_PropagatesValidationError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte(`
repos:
  rancher:
    kind: leaf
    deps: []
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAll(dir); err == nil || !strings.Contains(err.Error(), "repo is required") {
		t.Errorf("want validation error, got %v", err)
	}
}

func TestLoadAll_RejectsEmptyDir(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadAll(dir); err == nil || !strings.Contains(err.Error(), "no *.yaml files") {
		t.Errorf("want empty-dir error, got %v", err)
	}
}

func TestLoadAll_RejectsConfigWithoutLeaf(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "noleaf.yaml"), []byte(`
repos:
  wrangler:
    kind: independent
    repo: x/wrangler
    deps: []
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAll(dir); err == nil || !strings.Contains(err.Error(), "exactly one leaf") {
		t.Errorf("want one-leaf error, got %v", err)
	}
}

func TestFirstModulePath(t *testing.T) {
	cfg := &Config{
		Repos: map[string]Repo{
			"wrangler": {Repo: "rancher/wrangler"},
		},
		Modules: map[string][]string{
			"wrangler": {"github.com/rancher/wrangler"},
		},
	}
	if got := cfg.FirstModulePath("wrangler"); got != "github.com/rancher/wrangler" {
		t.Errorf("got %q", got)
	}
	if got := cfg.FirstModulePath("unknown"); got != "" {
		t.Errorf("unknown repo should return empty string, got %q", got)
	}
}
