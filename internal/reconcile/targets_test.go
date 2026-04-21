package reconcile

import (
	"reflect"
	"testing"

	"github.com/rancher/release-automation/internal/config"
)

func newCfg(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Repos: map[string]config.Repo{
			"rancher": {Kind: config.KindLeaf, Module: "github.com/rancher/rancher", Deps: []string{"steve", "wrangler"}},
			"steve":   {Kind: config.KindPaired, Module: "github.com/rancher/steve", Deps: []string{"wrangler"}},
			"wrangler": {Kind: config.KindIndependent, Module: "github.com/rancher/wrangler"},
		},
	}
}

func steveTable() *config.VersionTable {
	return &config.VersionTable{Rows: []config.VersionRow{
		{Branch: "main", Minor: "v0.9", Pair: "v2.15"},
		{Branch: "release/v0.8", Minor: "v0.8", Pair: "v2.14"},
		{Branch: "release/v0.7", Minor: "v0.7", Pair: "v2.13"},
	}}
}

func rancherTable() *config.VersionTable {
	// Rancher's table maps its own branches/minors. Pair column unused by
	// our algorithm (rancher is leaf), but we populate it for realism.
	return &config.VersionTable{Rows: []config.VersionRow{
		{Branch: "main", Minor: "v2.15"},
		{Branch: "release/v2.14", Minor: "v2.14"},
		{Branch: "release/v2.13", Minor: "v2.13"},
	}}
}

func TestComputeTargets_PairedSteveToRancherReleaseBranch(t *testing.T) {
	cfg := newCfg(t)
	got, err := ComputeTargets(cfg, "steve", "v0.7.5", steveTable(), map[string]*config.VersionTable{
		"rancher": rancherTable(),
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := []Target{{Repo: "rancher", Branch: "release/v2.13"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v want %+v", got, want)
	}
}

func TestComputeTargets_PairedSteveMainLineToRancherMain(t *testing.T) {
	cfg := newCfg(t)
	got, err := ComputeTargets(cfg, "steve", "v0.9.0", steveTable(), map[string]*config.VersionTable{
		"rancher": rancherTable(),
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := []Target{{Repo: "rancher", Branch: "main"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v want %+v", got, want)
	}
}

func TestComputeTargets_PairedDownstreamMissingMinor(t *testing.T) {
	cfg := newCfg(t)
	// Steve v0.6 pairs to rancher v2.12, but rancherTable doesn't include
	// release/v2.12 — should silently skip (e.g. rancher hasn't cut that
	// branch yet).
	steve := steveTable()
	steve.Rows = append(steve.Rows, config.VersionRow{Branch: "release/v0.6", Minor: "v0.6", Pair: "v2.12"})
	got, err := ComputeTargets(cfg, "steve", "v0.6.1", steve, map[string]*config.VersionTable{
		"rancher": rancherTable(),
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no targets, got %+v", got)
	}
}

func TestComputeTargets_IndependentTargetsMainOnly(t *testing.T) {
	cfg := newCfg(t)
	got, err := ComputeTargets(cfg, "wrangler", "v3.2.0", nil, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Order isn't guaranteed (map iteration). Sort by repo name for compare.
	want := map[string]string{"rancher": "main", "steve": "main"}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d want %d (got=%+v)", len(got), len(want), got)
	}
	for _, target := range got {
		if want[target.Repo] != target.Branch {
			t.Errorf("target %+v not in want %+v", target, want)
		}
	}
}

func TestComputeTargets_LeafEmits(t *testing.T) {
	cfg := newCfg(t)
	got, err := ComputeTargets(cfg, "rancher", "v2.15.0", nil, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("leaf should propagate nothing, got %+v", got)
	}
}

func TestComputeTargets_BadVersion(t *testing.T) {
	cfg := newCfg(t)
	_, err := ComputeTargets(cfg, "steve", "not-a-version", steveTable(), map[string]*config.VersionTable{
		"rancher": rancherTable(),
	})
	if err == nil {
		t.Fatal("expected error for invalid semver")
	}
}

func TestComputeTargets_PairedMissingDepTable(t *testing.T) {
	cfg := newCfg(t)
	_, err := ComputeTargets(cfg, "steve", "v0.7.5", nil, map[string]*config.VersionTable{
		"rancher": rancherTable(),
	})
	if err == nil {
		t.Fatal("expected error for missing dep VERSION.md")
	}
}

// --- ComputeTargetsForLeafBranch -----------------------------------------

func sortTargets(ts []Target) []Target {
	out := append([]Target(nil), ts...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && (out[j-1].Repo > out[j].Repo || (out[j-1].Repo == out[j].Repo && out[j-1].Branch > out[j].Branch)); j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

func TestComputeTargetsForLeafBranch_FansOutLeafAndPaired(t *testing.T) {
	cfg := newCfg(t)
	got, err := ComputeTargetsForLeafBranch(cfg, "wrangler", "rancher", "release/v2.13",
		rancherTable(), map[string]*config.VersionTable{"steve": steveTable()})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := []Target{
		{Repo: "rancher", Branch: "release/v2.13"},
		{Repo: "steve", Branch: "release/v0.7"},
	}
	if !reflect.DeepEqual(sortTargets(got), want) {
		t.Errorf("got %+v want %+v", got, want)
	}
}

func TestComputeTargetsForLeafBranch_MainLine(t *testing.T) {
	cfg := newCfg(t)
	got, err := ComputeTargetsForLeafBranch(cfg, "wrangler", "rancher", "main",
		rancherTable(), map[string]*config.VersionTable{"steve": steveTable()})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := []Target{
		{Repo: "rancher", Branch: "main"},
		{Repo: "steve", Branch: "main"},
	}
	if !reflect.DeepEqual(sortTargets(got), want) {
		t.Errorf("got %+v want %+v", got, want)
	}
}

func TestComputeTargetsForLeafBranch_PairedDownstreamSkipsUnknownPair(t *testing.T) {
	cfg := newCfg(t)
	// rancher knows release/v2.14 but steve has no row pairing to v2.14
	// (steveTable has v2.15/v2.13/v2.12 but not v2.14 in the wrangler-deps
	// fixture). Build a steve table that explicitly omits v2.14.
	steve := &config.VersionTable{Rows: []config.VersionRow{
		{Branch: "main", Minor: "v0.9", Pair: "v2.15"},
		{Branch: "release/v0.7", Minor: "v0.7", Pair: "v2.13"},
	}}
	got, err := ComputeTargetsForLeafBranch(cfg, "wrangler", "rancher", "release/v2.14",
		rancherTable(), map[string]*config.VersionTable{"steve": steve})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := []Target{{Repo: "rancher", Branch: "release/v2.14"}}
	if !reflect.DeepEqual(sortTargets(got), want) {
		t.Errorf("got %+v want %+v", got, want)
	}
}

func TestComputeTargetsForLeafBranch_UnknownLeafBranch(t *testing.T) {
	cfg := newCfg(t)
	_, err := ComputeTargetsForLeafBranch(cfg, "wrangler", "rancher", "release/v9.9",
		rancherTable(), map[string]*config.VersionTable{"steve": steveTable()})
	if err == nil {
		t.Fatal("expected error for branch not in leaf VERSION.md")
	}
}

func TestComputeTargetsForLeafBranch_RejectsNonLeaf(t *testing.T) {
	cfg := newCfg(t)
	_, err := ComputeTargetsForLeafBranch(cfg, "wrangler", "steve", "main",
		steveTable(), nil)
	if err == nil {
		t.Fatal("expected error when leafRepo isn't kind=leaf")
	}
}
