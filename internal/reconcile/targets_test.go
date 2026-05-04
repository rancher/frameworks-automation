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
			"rancher":  {Kind: config.KindLeaf, Repo: "rancher/rancher", Deps: []config.Dep{{Name: "steve", Strategy: config.StrategyGoGet}, {Name: "wrangler", Strategy: config.StrategyGoGet}}},
			"steve":    {Kind: config.KindPaired, Repo: "rancher/steve", Deps: []config.Dep{{Name: "wrangler", Strategy: config.StrategyGoGet}}},
			"wrangler": {Kind: config.KindIndependent, Repo: "rancher/wrangler"},
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
