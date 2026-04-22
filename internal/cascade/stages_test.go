package cascade

import (
	"reflect"
	"testing"

	"github.com/rancher/release-automation/internal/config"
)

func newCfg() *config.Config {
	return &config.Config{
		Repos: map[string]config.Repo{
			"rancher":  {Kind: config.KindLeaf, Module: "github.com/x/rancher", Deps: []string{"steve", "wrangler"}},
			"steve":    {Kind: config.KindPaired, Module: "github.com/x/steve", Deps: []string{"wrangler"}},
			"wrangler": {Kind: config.KindIndependent, Module: "github.com/x/wrangler"},
		},
	}
}

func rancherTable() *config.VersionTable {
	return &config.VersionTable{Rows: []config.VersionRow{
		{Branch: "main", Minor: "v2.15"},
		{Branch: "release/v2.14", Minor: "v2.14"},
		{Branch: "release/v2.13", Minor: "v2.13"},
	}}
}

func steveTable() *config.VersionTable {
	return &config.VersionTable{Rows: []config.VersionRow{
		{Branch: "main", Minor: "v0.9", Pair: "v2.15"},
		{Branch: "release/v0.8", Minor: "v0.8", Pair: "v2.14"},
		{Branch: "release/v0.7", Minor: "v0.7", Pair: "v2.13"},
	}}
}

func TestComputeStages_LinearChain(t *testing.T) {
	cfg := newCfg()
	stages, err := ComputeStages(cfg, "wrangler", "v0.5.1", "rancher", "main",
		rancherTable(), map[string]*config.VersionTable{"steve": steveTable()})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(stages) != 2 {
		t.Fatalf("want 2 stages, got %d: %+v", len(stages), stages)
	}
	// Stage 1: bump wrangler in steve main, prompt steve tag.
	want1 := Stage{
		Layer: 1,
		Bumps: []Bump{{Repo: "steve", Branch: "main", Dep: "wrangler", Module: "github.com/x/wrangler", Version: "v0.5.1"}},
		Tags:  []TagPrompt{{Repo: "steve", Branch: "main"}},
	}
	if !reflect.DeepEqual(stages[0], want1) {
		t.Errorf("stage 1: got %+v want %+v", stages[0], want1)
	}
	// Stage 2 (final): bump steve AND wrangler into rancher main, no tag prompt.
	want2 := Stage{
		Layer: 2,
		Bumps: []Bump{
			{Repo: "rancher", Branch: "main", Dep: "steve", Module: "github.com/x/steve"},
			{Repo: "rancher", Branch: "main", Dep: "wrangler", Module: "github.com/x/wrangler", Version: "v0.5.1"},
		},
	}
	if !reflect.DeepEqual(stages[1], want2) {
		t.Errorf("stage 2: got %+v want %+v", stages[1], want2)
	}
}

func TestComputeStages_PairedReleaseBranch(t *testing.T) {
	cfg := newCfg()
	stages, err := ComputeStages(cfg, "wrangler", "v0.5.1", "rancher", "release/v2.13",
		rancherTable(), map[string]*config.VersionTable{"steve": steveTable()})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(stages) != 2 {
		t.Fatalf("want 2 stages, got %d", len(stages))
	}
	if stages[0].Bumps[0].Repo != "steve" || stages[0].Bumps[0].Branch != "release/v0.7" {
		t.Errorf("stage 1 should target steve release/v0.7: %+v", stages[0].Bumps)
	}
	if stages[0].Tags[0].Branch != "release/v0.7" {
		t.Errorf("stage 1 tag prompt should be on release/v0.7: %+v", stages[0].Tags)
	}
	if stages[1].Bumps[0].Repo != "rancher" || stages[1].Bumps[0].Branch != "release/v2.13" {
		t.Errorf("stage 2 should target rancher release/v2.13: %+v", stages[1].Bumps)
	}
}

func TestComputeStages_DirectLeafDepOnly(t *testing.T) {
	// rancher → wrangler directly, no intermediate. Cascade is one stage.
	cfg := &config.Config{
		Repos: map[string]config.Repo{
			"rancher":  {Kind: config.KindLeaf, Module: "github.com/x/rancher", Deps: []string{"wrangler"}},
			"wrangler": {Kind: config.KindIndependent, Module: "github.com/x/wrangler"},
		},
	}
	stages, err := ComputeStages(cfg, "wrangler", "v0.5.1", "rancher", "main", rancherTable(), nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(stages) != 1 {
		t.Fatalf("want 1 stage, got %d: %+v", len(stages), stages)
	}
	if len(stages[0].Tags) != 0 {
		t.Errorf("final stage should have no tag prompts: %+v", stages[0].Tags)
	}
	if len(stages[0].Bumps) != 1 || stages[0].Bumps[0].Repo != "rancher" {
		t.Errorf("expected single rancher bump: %+v", stages[0].Bumps)
	}
}

func TestComputeStages_FanInDAG(t *testing.T) {
	// D is the source. A (leaf) depends on B, C, D. B and C depend on D.
	// Both B and C land in stage 1; A's stage 2 bumps both B and C (D comes
	// in via either's transitive go.mod, but we list every direct in-scope
	// dep so each layer CIs against the new dep explicitly).
	cfg := &config.Config{
		Repos: map[string]config.Repo{
			"A": {Kind: config.KindLeaf, Module: "github.com/x/a", Deps: []string{"B", "C", "D"}},
			"B": {Kind: config.KindIndependent, Module: "github.com/x/b", Deps: []string{"D"}},
			"C": {Kind: config.KindIndependent, Module: "github.com/x/c", Deps: []string{"D"}},
			"D": {Kind: config.KindIndependent, Module: "github.com/x/d"},
		},
	}
	aTable := &config.VersionTable{Rows: []config.VersionRow{{Branch: "main", Minor: "v1"}}}
	stages, err := ComputeStages(cfg, "D", "vNEW", "A", "main", aTable, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(stages) != 2 {
		t.Fatalf("want 2 stages, got %d: %+v", len(stages), stages)
	}
	// Stage 1: B + C (sorted), each bumping D, with tag prompts for both.
	if len(stages[0].Bumps) != 2 || stages[0].Bumps[0].Repo != "B" || stages[0].Bumps[1].Repo != "C" {
		t.Errorf("stage 1 bumps: %+v", stages[0].Bumps)
	}
	if len(stages[0].Tags) != 2 {
		t.Errorf("stage 1 tag prompts: %+v", stages[0].Tags)
	}
	// Stage 2: A bumping B, C, D (sorted; D=source has Version pre-filled).
	if len(stages[1].Bumps) != 3 {
		t.Fatalf("stage 2 bumps len: %+v", stages[1].Bumps)
	}
	gotDeps := []string{stages[1].Bumps[0].Dep, stages[1].Bumps[1].Dep, stages[1].Bumps[2].Dep}
	want := []string{"B", "C", "D"}
	if !reflect.DeepEqual(gotDeps, want) {
		t.Errorf("stage 2 dep order: got %v want %v", gotDeps, want)
	}
	// D bump in stage 2 has the source version; B and C don't yet.
	for _, b := range stages[1].Bumps {
		switch b.Dep {
		case "D":
			if b.Version != "vNEW" {
				t.Errorf("D bump should have source version: %+v", b)
			}
		case "B", "C":
			if b.Version != "" {
				t.Errorf("%s bump should be empty until tag arrives: %+v", b.Dep, b)
			}
		}
	}
}

func TestComputeStages_NoPathToLeaf(t *testing.T) {
	cfg := &config.Config{
		Repos: map[string]config.Repo{
			"rancher":  {Kind: config.KindLeaf, Module: "github.com/x/rancher", Deps: []string{"steve"}},
			"steve":    {Kind: config.KindPaired, Module: "github.com/x/steve"},
			"wrangler": {Kind: config.KindIndependent, Module: "github.com/x/wrangler"},
		},
	}
	_, err := ComputeStages(cfg, "wrangler", "v0.5.1", "rancher", "main", rancherTable(), nil)
	if err == nil {
		t.Fatal("expected error: no path from wrangler to rancher")
	}
}

func TestComputeStages_PairedMissingTable(t *testing.T) {
	cfg := newCfg()
	_, err := ComputeStages(cfg, "wrangler", "v0.5.1", "rancher", "main", rancherTable(), nil)
	if err == nil {
		t.Fatal("expected error when paired dependent has no VERSION.md table")
	}
}

func TestComputeStages_PairedNoMatchingPair(t *testing.T) {
	cfg := newCfg()
	steve := &config.VersionTable{Rows: []config.VersionRow{
		{Branch: "main", Minor: "v0.9", Pair: "v2.15"},
		// no row pairing v2.13
	}}
	_, err := ComputeStages(cfg, "wrangler", "v0.5.1", "rancher", "release/v2.13",
		rancherTable(), map[string]*config.VersionTable{"steve": steve})
	if err == nil {
		t.Fatal("expected error when paired dep has no row for leaf minor")
	}
}

func TestComputeStages_RejectsNonLeaf(t *testing.T) {
	cfg := newCfg()
	_, err := ComputeStages(cfg, "wrangler", "v0.5.1", "steve", "main", steveTable(), nil)
	if err == nil {
		t.Fatal("expected error when leafRepo isn't kind=leaf")
	}
}

func TestComputeStages_LeafBranchNotInTable(t *testing.T) {
	cfg := newCfg()
	_, err := ComputeStages(cfg, "wrangler", "v0.5.1", "rancher", "release/v9.9",
		rancherTable(), map[string]*config.VersionTable{"steve": steveTable()})
	if err == nil {
		t.Fatal("expected error for branch not in leaf VERSION.md")
	}
}
