package cascade

import (
	"reflect"
	"testing"

	"github.com/rancher/release-automation/internal/config"
)

func newCfg() *config.Config {
	return &config.Config{
		Repos: map[string]config.Repo{
			"rancher":  {Kind: config.KindLeaf, Module: "github.com/x/rancher", Deps: deps("steve", "wrangler")},
			"steve":    {Kind: config.KindPaired, Module: "github.com/x/steve", Deps: deps("wrangler")},
			"wrangler": {Kind: config.KindIndependent, Module: "github.com/x/wrangler"},
		},
	}
}

// deps is a tiny helper for fixtures: each name becomes a Dep with the
// default StrategyGoGet, matching what applyDefaults does at load time.
func deps(names ...string) []config.Dep {
	out := make([]config.Dep, len(names))
	for i, n := range names {
		out[i] = config.Dep{Name: n, Strategy: config.StrategyGoGet}
	}
	return out
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

// nilResolver fails if called — useful when the test expects no paired-latest
// lookup (e.g. all paired deps are in propagation, so they're re-tagged).
func nilResolver(repo, branch string) (string, error) {
	return "", nil
}

// fixedResolver returns a constant version for any repo/branch lookup.
func fixedResolver(versions map[string]string) LatestResolver {
	return func(repo, branch string) (string, error) {
		if v, ok := versions[repo]; ok {
			return v, nil
		}
		return "", nil
	}
}

func TestComputeStages_LinearChain(t *testing.T) {
	cfg := newCfg()
	sources, stages, err := ComputeStages(cfg,
		map[string]string{"wrangler": "v0.5.1"},
		"rancher", "main",
		rancherTable(),
		map[string]*config.VersionTable{"steve": steveTable()},
		nilResolver, nil,
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	wantSources := []Source{{Name: "wrangler", Version: "v0.5.1", Explicit: true}}
	if !reflect.DeepEqual(sources, wantSources) {
		t.Errorf("sources: got %+v want %+v", sources, wantSources)
	}
	if len(stages) != 2 {
		t.Fatalf("want 2 stages, got %d: %+v", len(stages), stages)
	}
	// Stage 1: bump wrangler in steve main, prompt steve tag.
	want1 := Stage{
		Layer: 1,
		Bumps: []Bump{{Repo: "steve", Branch: "main",
			Deps: []DepBump{{Dep: "wrangler", Module: "github.com/x/wrangler", Version: "v0.5.1", Strategy: config.StrategyGoGet}}}},
		Tags: []TagPrompt{{Repo: "steve", Branch: "main"}},
	}
	if !reflect.DeepEqual(stages[0], want1) {
		t.Errorf("stage 1: got %+v want %+v", stages[0], want1)
	}
	// Stage 2 (final): bump steve AND wrangler into rancher main, no tag
	// prompt. One Bump bundling both deps — wrangler pre-filled (source dep),
	// steve empty until the stage-1 tag arrives.
	want2 := Stage{
		Layer: 2,
		Bumps: []Bump{{Repo: "rancher", Branch: "main",
			Deps: []DepBump{
				{Dep: "steve", Module: "github.com/x/steve", Strategy: config.StrategyGoGet},
				{Dep: "wrangler", Module: "github.com/x/wrangler", Version: "v0.5.1", Strategy: config.StrategyGoGet},
			}}},
	}
	if !reflect.DeepEqual(stages[1], want2) {
		t.Errorf("stage 2: got %+v want %+v", stages[1], want2)
	}
}

func TestComputeStages_PairedReleaseBranch(t *testing.T) {
	cfg := newCfg()
	_, stages, err := ComputeStages(cfg,
		map[string]string{"wrangler": "v0.5.1"},
		"rancher", "release/v2.13",
		rancherTable(),
		map[string]*config.VersionTable{"steve": steveTable()},
		nilResolver, nil,
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(stages) != 2 {
		t.Fatalf("want 2 stages, got %d", len(stages))
	}
	if stages[0].Bumps[0].Repo != "steve" || stages[0].Bumps[0].Branch != "release/v0.7" {
		t.Errorf("stage 1 should target steve release/v0.7: %+v", stages[0].Bumps)
	}
	if len(stages[0].Bumps[0].Deps) != 1 || stages[0].Bumps[0].Deps[0].Dep != "wrangler" {
		t.Errorf("stage 1 bump should bundle wrangler: %+v", stages[0].Bumps[0].Deps)
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
			"rancher":  {Kind: config.KindLeaf, Module: "github.com/x/rancher", Deps: deps("wrangler")},
			"wrangler": {Kind: config.KindIndependent, Module: "github.com/x/wrangler"},
		},
	}
	_, stages, err := ComputeStages(cfg,
		map[string]string{"wrangler": "v0.5.1"},
		"rancher", "main",
		rancherTable(), nil, nilResolver, nil,
	)
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
			"A": {Kind: config.KindLeaf, Module: "github.com/x/a", Deps: deps("B", "C", "D")},
			"B": {Kind: config.KindIndependent, Module: "github.com/x/b", Deps: deps("D")},
			"C": {Kind: config.KindIndependent, Module: "github.com/x/c", Deps: deps("D")},
			"D": {Kind: config.KindIndependent, Module: "github.com/x/d"},
		},
	}
	aTable := &config.VersionTable{Rows: []config.VersionRow{{Branch: "main", Minor: "v1"}}}
	_, stages, err := ComputeStages(cfg,
		map[string]string{"D": "vNEW"},
		"A", "main",
		aTable, nil, nilResolver, nil,
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(stages) != 2 {
		t.Fatalf("want 2 stages, got %d: %+v", len(stages), stages)
	}
	// Stage 1: B + C (sorted), each a one-(repo, branch) Bump bumping D, with
	// tag prompts for both.
	if len(stages[0].Bumps) != 2 || stages[0].Bumps[0].Repo != "B" || stages[0].Bumps[1].Repo != "C" {
		t.Errorf("stage 1 bumps: %+v", stages[0].Bumps)
	}
	for _, bp := range stages[0].Bumps {
		if len(bp.Deps) != 1 || bp.Deps[0].Dep != "D" || bp.Deps[0].Version != "vNEW" {
			t.Errorf("stage 1 %s bump should bundle D@vNEW: %+v", bp.Repo, bp.Deps)
		}
	}
	if len(stages[0].Tags) != 2 {
		t.Errorf("stage 1 tag prompts: %+v", stages[0].Tags)
	}
	// Stage 2: one A Bump bundling B, C, D (sorted; D=source has Version
	// pre-filled, B and C empty until stage-1 tags arrive).
	if len(stages[1].Bumps) != 1 {
		t.Fatalf("stage 2 should be one Bump for A: %+v", stages[1].Bumps)
	}
	a := stages[1].Bumps[0]
	if a.Repo != "A" || len(a.Deps) != 3 {
		t.Fatalf("stage 2 bump: %+v", a)
	}
	gotDeps := []string{a.Deps[0].Dep, a.Deps[1].Dep, a.Deps[2].Dep}
	want := []string{"B", "C", "D"}
	if !reflect.DeepEqual(gotDeps, want) {
		t.Errorf("stage 2 dep order: got %v want %v", gotDeps, want)
	}
	for _, d := range a.Deps {
		switch d.Dep {
		case "D":
			if d.Version != "vNEW" {
				t.Errorf("D entry should have source version: %+v", d)
			}
		case "B", "C":
			if d.Version != "" {
				t.Errorf("%s entry should be empty until tag arrives: %+v", d.Dep, d)
			}
		}
	}
}

func TestComputeStages_NoExplicitPullsPairedLatest(t *testing.T) {
	// Empty independents: cascade still picks up paired (steve) at latest tag
	// for the leaf-paired branch. One stage, final, bumps steve into rancher.
	cfg := newCfg()
	resolver := fixedResolver(map[string]string{"steve": "v0.9.4"})
	sources, stages, err := ComputeStages(cfg,
		map[string]string{},
		"rancher", "main",
		rancherTable(),
		map[string]*config.VersionTable{"steve": steveTable()},
		resolver, nil,
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(sources) != 1 || sources[0].Name != "steve" || sources[0].Version != "v0.9.4" || sources[0].Explicit {
		t.Errorf("sources: got %+v want one paired-latest steve@v0.9.4", sources)
	}
	if len(stages) != 1 {
		t.Fatalf("want 1 stage (final), got %d: %+v", len(stages), stages)
	}
	if len(stages[0].Tags) != 0 {
		t.Errorf("final stage should have no tag prompts: %+v", stages[0].Tags)
	}
	bp := stages[0].Bumps[0]
	if bp.Repo != "rancher" || len(bp.Deps) != 1 || bp.Deps[0].Dep != "steve" || bp.Deps[0].Version != "v0.9.4" {
		t.Errorf("expected single rancher bump bundling steve@v0.9.4: %+v", bp)
	}
}

func TestComputeStages_PairedLatestAlongsideExplicit(t *testing.T) {
	// rancher depends on steve (paired) and wrangler (independent), AND on
	// webhook (paired) which has nothing to do with wrangler. Cascade with
	// wrangler explicit:
	//   - steve is in propagation (transitively depends on wrangler) → re-cut.
	//   - webhook is paired but NOT in propagation → paired-latest source.
	// rancher's bundle: steve (empty until stage-1 tag), wrangler (explicit),
	// webhook (paired-latest pinned now).
	cfg := &config.Config{
		Repos: map[string]config.Repo{
			"rancher":  {Kind: config.KindLeaf, Module: "github.com/x/rancher", Deps: deps("steve", "wrangler", "webhook")},
			"steve":    {Kind: config.KindPaired, Module: "github.com/x/steve", Deps: deps("wrangler")},
			"webhook":  {Kind: config.KindPaired, Module: "github.com/x/webhook"},
			"wrangler": {Kind: config.KindIndependent, Module: "github.com/x/wrangler"},
		},
	}
	webhookTable := &config.VersionTable{Rows: []config.VersionRow{
		{Branch: "main", Minor: "v0.7", Pair: "v2.15"},
	}}
	resolver := fixedResolver(map[string]string{"webhook": "v0.7.4"})
	sources, stages, err := ComputeStages(cfg,
		map[string]string{"wrangler": "v0.5.1"},
		"rancher", "main",
		rancherTable(),
		map[string]*config.VersionTable{"steve": steveTable(), "webhook": webhookTable},
		resolver, nil,
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	hasExplicitWrangler := false
	hasPairedWebhook := false
	for _, s := range sources {
		if s.Name == "wrangler" && s.Version == "v0.5.1" && s.Explicit {
			hasExplicitWrangler = true
		}
		if s.Name == "webhook" && s.Version == "v0.7.4" && !s.Explicit {
			hasPairedWebhook = true
		}
	}
	if !hasExplicitWrangler || !hasPairedWebhook {
		t.Errorf("sources missing wrangler-explicit or webhook-paired-latest: %+v", sources)
	}
	if len(stages) != 2 {
		t.Fatalf("want 2 stages, got %d: %+v", len(stages), stages)
	}
	// Stage 2 (final): rancher bundle should include all three deps.
	rancherBundle := stages[1].Bumps[0].Deps
	if len(rancherBundle) != 3 {
		t.Fatalf("rancher bundle: got %d deps, want 3: %+v", len(rancherBundle), rancherBundle)
	}
	for _, d := range rancherBundle {
		switch d.Dep {
		case "wrangler":
			if d.Version != "v0.5.1" {
				t.Errorf("wrangler should be explicit: %+v", d)
			}
		case "webhook":
			if d.Version != "v0.7.4" {
				t.Errorf("webhook should be paired-latest: %+v", d)
			}
		case "steve":
			if d.Version != "" {
				t.Errorf("steve should be empty until tag arrives: %+v", d)
			}
		}
	}
}

func TestComputeStages_NoPathToLeafSkipsExplicit(t *testing.T) {
	// wrangler has no path to rancher → no propagation. With no paired deps
	// either, there's nothing to bump and ComputeStages errors.
	cfg := &config.Config{
		Repos: map[string]config.Repo{
			"rancher":  {Kind: config.KindLeaf, Module: "github.com/x/rancher", Deps: deps("steve")},
			"steve":    {Kind: config.KindPaired, Module: "github.com/x/steve"}, // doesn't depend on wrangler
			"wrangler": {Kind: config.KindIndependent, Module: "github.com/x/wrangler"},
		},
	}
	steveTbl := &config.VersionTable{Rows: []config.VersionRow{{Branch: "main", Minor: "v0.9", Pair: "v2.15"}}}
	resolver := fixedResolver(map[string]string{"steve": "v0.9.4"})
	// wrangler explicit but no path → cascade still runs because steve
	// (paired) is auto-bumped. wrangler is essentially ignored (not in any
	// stage repo's bundle).
	sources, stages, err := ComputeStages(cfg,
		map[string]string{"wrangler": "v0.5.1"},
		"rancher", "main",
		rancherTable(),
		map[string]*config.VersionTable{"steve": steveTbl},
		resolver, nil,
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// wrangler is still listed as a source (user supplied it) but doesn't
	// appear in any bundle since no stage repo depends on it.
	if len(stages) != 1 {
		t.Fatalf("want 1 stage, got %d", len(stages))
	}
	bundle := stages[0].Bumps[0].Deps
	for _, d := range bundle {
		if d.Dep == "wrangler" {
			t.Errorf("wrangler shouldn't be in rancher bundle (no path): %+v", bundle)
		}
	}
	hasWrangler := false
	for _, s := range sources {
		if s.Name == "wrangler" {
			hasWrangler = true
		}
	}
	if !hasWrangler {
		t.Errorf("wrangler source should still be listed even when out of scope: %+v", sources)
	}
}

func TestComputeStages_PairedMissingTable(t *testing.T) {
	cfg := newCfg()
	_, _, err := ComputeStages(cfg,
		map[string]string{"wrangler": "v0.5.1"},
		"rancher", "main",
		rancherTable(), nil, nilResolver, nil,
	)
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
	_, _, err := ComputeStages(cfg,
		map[string]string{"wrangler": "v0.5.1"},
		"rancher", "release/v2.13",
		rancherTable(),
		map[string]*config.VersionTable{"steve": steve},
		nilResolver, nil,
	)
	if err == nil {
		t.Fatal("expected error when paired dep has no row for leaf minor")
	}
}

func TestComputeStages_RejectsNonLeaf(t *testing.T) {
	cfg := newCfg()
	_, _, err := ComputeStages(cfg,
		map[string]string{"wrangler": "v0.5.1"},
		"steve", "main",
		steveTable(), nil, nilResolver, nil,
	)
	if err == nil {
		t.Fatal("expected error when leafRepo isn't kind=leaf")
	}
}

func TestComputeStages_LeafBranchNotInTable(t *testing.T) {
	cfg := newCfg()
	_, _, err := ComputeStages(cfg,
		map[string]string{"wrangler": "v0.5.1"},
		"rancher", "release/v9.9",
		rancherTable(),
		map[string]*config.VersionTable{"steve": steveTable()},
		nilResolver, nil,
	)
	if err == nil {
		t.Fatal("expected error for branch not in leaf VERSION.md")
	}
}

// chartLikeCfg sets up the playground topology with a chart sibling:
//
//	wrangler → webhook → chart   (chart-bump strategy)
//	wrangler → webhook → rancher (bump-webhook strategy)
//	rancher  → chart              (order strategy; sequencing only)
//
// Both rancher and chart consume webhook with non-go-get strategies; chart
// has no Go module and uses a branch-template instead of VERSION.md.
func chartLikeCfg() *config.Config {
	return &config.Config{Repos: map[string]config.Repo{
		"rancher": {Kind: config.KindLeaf, Module: "github.com/x/rancher", Deps: []config.Dep{
			{Name: "webhook", Strategy: config.StrategyBumpWebhook},
			{Name: "chart", Strategy: config.StrategyOrder},
		}},
		"chart": {Kind: config.KindPaired, Module: "github.com/x/chart",
			BranchTemplate: "dev-{rancher-minor}",
			Deps:           []config.Dep{{Name: "webhook", Strategy: config.StrategyChartBump}}},
		"webhook":  {Kind: config.KindPaired, Module: "github.com/x/webhook", Deps: deps("wrangler")},
		"wrangler": {Kind: config.KindIndependent, Module: "github.com/x/wrangler"},
	}}
}

func TestComputeStages_OrderEdgeSequencesChartBeforeRancher(t *testing.T) {
	cfg := chartLikeCfg()
	webhookTbl := &config.VersionTable{Rows: []config.VersionRow{
		{Branch: "main", Minor: "v0.7", Pair: "v2.15"},
	}}
	// wrangler explicit drives propagation through webhook → {chart, rancher}.
	// Stage 1: webhook bumps wrangler (paired-leaf). Stage 2: chart bumps
	// webhook (chart-bump). Stage 3: rancher bumps webhook (bump-webhook),
	// gated on chart's stage 2 finishing via the order edge.
	_, stages, err := ComputeStages(cfg,
		map[string]string{"wrangler": "v0.5.6"},
		"rancher", "main", rancherTable(),
		map[string]*config.VersionTable{"webhook": webhookTbl},
		nilResolver, nil,
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(stages) != 3 {
		t.Fatalf("want 3 stages (webhook, chart, rancher), got %d: %+v", len(stages), stages)
	}
	// Stage 1: webhook bump on its main, with a tag prompt (chart and rancher
	// both consume its version).
	st1 := stages[0]
	if len(st1.Bumps) != 1 || st1.Bumps[0].Repo != "webhook" {
		t.Fatalf("stage 1 should bump webhook: %+v", st1.Bumps)
	}
	if len(st1.Tags) != 1 || st1.Tags[0].Repo != "webhook" {
		t.Errorf("stage 1 should have a webhook tag prompt: %+v", st1.Tags)
	}
	// Stage 2: chart bump on dev-v2.15 (template-resolved), no tag prompt
	// (rancher only consumes chart via order — no version needed).
	st2 := stages[1]
	if len(st2.Bumps) != 1 || st2.Bumps[0].Repo != "chart" {
		t.Fatalf("stage 2 should bump chart: %+v", st2.Bumps)
	}
	if st2.Bumps[0].Branch != "dev-v2.15" {
		t.Errorf("chart branch should come from branch-template: got %q", st2.Bumps[0].Branch)
	}
	if len(st2.Bumps[0].Deps) != 1 || st2.Bumps[0].Deps[0].Dep != "webhook" || st2.Bumps[0].Deps[0].Strategy != config.StrategyChartBump {
		t.Errorf("chart bundle should be webhook with chart-bump strategy: %+v", st2.Bumps[0].Deps)
	}
	if len(st2.Tags) != 0 {
		t.Errorf("chart stage should be bump-only (rancher consumes chart only via order): %+v", st2.Tags)
	}
	// Stage 3 (final): rancher bumps webhook with bump-webhook strategy. No
	// chart entry in the bundle (order edge dropped).
	st3 := stages[2]
	if len(st3.Bumps) != 1 || st3.Bumps[0].Repo != "rancher" {
		t.Fatalf("stage 3 should bump rancher: %+v", st3.Bumps)
	}
	for _, d := range st3.Bumps[0].Deps {
		if d.Dep == "chart" {
			t.Errorf("rancher bundle must not include chart (order edge): %+v", st3.Bumps[0].Deps)
		}
		if d.Dep == "webhook" && d.Strategy != config.StrategyBumpWebhook {
			t.Errorf("rancher's webhook entry should carry bump-webhook: %+v", d)
		}
	}
}

func TestComputeStages_StalePairedPromotedIntoPropagation(t *testing.T) {
	// steve is stale (unreleased commits). With no explicit independents, a
	// normal cascade would put steve in paired-latest and emit one final stage
	// bumping only rancher. With steve in staleRepos, it joins the propagation
	// set: the cascade should produce a stage-1 tag prompt for steve followed
	// by a final bump of rancher that uses steve's new tag.
	cfg := newCfg()
	stale := map[string]bool{"steve": true}
	sources, stages, err := ComputeStages(cfg,
		map[string]string{},
		"rancher", "main",
		rancherTable(),
		map[string]*config.VersionTable{"steve": steveTable()},
		nilResolver, stale,
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// steve should NOT appear as a paired-latest source — it's in propagation.
	for _, s := range sources {
		if s.Name == "steve" {
			t.Errorf("steve should not be a paired-latest source when stale: %+v", sources)
		}
	}
	if len(stages) != 2 {
		t.Fatalf("want 2 stages (steve tag-only, rancher final), got %d: %+v", len(stages), stages)
	}
	// Stage 1: no bumps (wrangler is out-of-scope), one tag prompt for steve.
	st1 := stages[0]
	if len(st1.Bumps) != 0 {
		t.Errorf("stage 1 should have no bumps (no in-scope deps): %+v", st1.Bumps)
	}
	if len(st1.Tags) != 1 || st1.Tags[0].Repo != "steve" {
		t.Errorf("stage 1 should have a steve tag prompt: %+v", st1.Tags)
	}
	// Stage 2 (final): bump rancher with steve (version pending from tag).
	st2 := stages[1]
	if len(st2.Bumps) != 1 || st2.Bumps[0].Repo != "rancher" {
		t.Fatalf("stage 2 should bump rancher: %+v", st2.Bumps)
	}
	found := false
	for _, d := range st2.Bumps[0].Deps {
		if d.Dep == "steve" {
			found = true
			if d.Version != "" {
				t.Errorf("steve dep should be empty until tag arrives: %+v", d)
			}
		}
	}
	if !found {
		t.Errorf("rancher bundle should include steve dep: %+v", st2.Bumps[0].Deps)
	}
}

func TestComputeStages_StaleWebhookProducesChartStage(t *testing.T) {
	// webhook is stale (unreleased commits). With no explicit independents the
	// normal cascade resolves webhook as paired-latest and emits a single rancher
	// bump. With webhook in staleRepos, the forward closure also pulls chart into
	// propagation (chart depends on webhook), so the cascade should produce:
	//   stage 1: webhook tag prompt (no in-scope bump deps)
	//   stage 2: chart bump (webhook dep, empty version until tag arrives)
	//   stage 3 (final): rancher bump (webhook dep, empty version)
	cfg := chartLikeCfg()
	webhookTbl := &config.VersionTable{Rows: []config.VersionRow{
		{Branch: "main", Minor: "v0.7", Pair: "v2.15"},
	}}
	stale := map[string]bool{"webhook": true}
	_, stages, err := ComputeStages(cfg,
		map[string]string{},
		"rancher", "main",
		rancherTable(),
		map[string]*config.VersionTable{"webhook": webhookTbl},
		nilResolver, stale,
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(stages) != 3 {
		t.Fatalf("want 3 stages (webhook tag-only, chart bump, rancher final), got %d: %+v", len(stages), stages)
	}
	// Stage 1: no bumps (wrangler out-of-scope), tag prompt for webhook.
	st1 := stages[0]
	if len(st1.Bumps) != 0 {
		t.Errorf("stage 1 should have no bumps: %+v", st1.Bumps)
	}
	if len(st1.Tags) != 1 || st1.Tags[0].Repo != "webhook" {
		t.Errorf("stage 1 should have a webhook tag prompt: %+v", st1.Tags)
	}
	// Stage 2: chart bump with empty webhook version (pending tag), no tag prompt.
	st2 := stages[1]
	if len(st2.Bumps) != 1 || st2.Bumps[0].Repo != "chart" {
		t.Fatalf("stage 2 should bump chart: %+v", st2.Bumps)
	}
	if len(st2.Bumps[0].Deps) != 1 || st2.Bumps[0].Deps[0].Dep != "webhook" || st2.Bumps[0].Deps[0].Version != "" {
		t.Errorf("chart bundle should have webhook with empty version: %+v", st2.Bumps[0].Deps)
	}
	if len(st2.Tags) != 0 {
		t.Errorf("chart stage should be bump-only (rancher consumes chart only via order): %+v", st2.Tags)
	}
	// Stage 3 (final): rancher bumps webhook (empty version).
	st3 := stages[2]
	if len(st3.Bumps) != 1 || st3.Bumps[0].Repo != "rancher" {
		t.Fatalf("stage 3 should bump rancher: %+v", st3.Bumps)
	}
	foundWebhook := false
	for _, d := range st3.Bumps[0].Deps {
		if d.Dep == "webhook" {
			foundWebhook = true
			if d.Version != "" {
				t.Errorf("webhook dep in rancher should be empty until tag arrives: %+v", d)
			}
		}
		if d.Dep == "chart" {
			t.Errorf("rancher bundle must not include chart (order edge): %+v", d)
		}
	}
	if !foundWebhook {
		t.Errorf("rancher bundle should include webhook dep: %+v", st3.Bumps[0].Deps)
	}
}

func TestComputeStages_RejectsPairedAsExplicit(t *testing.T) {
	cfg := newCfg()
	_, _, err := ComputeStages(cfg,
		map[string]string{"steve": "v0.9.4"},
		"rancher", "main",
		rancherTable(),
		map[string]*config.VersionTable{"steve": steveTable()},
		nilResolver, nil,
	)
	if err == nil {
		t.Fatal("expected error when a paired dep is supplied as an explicit source")
	}
}
