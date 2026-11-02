package reconcile

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/rancher/release-automation/internal/cascade"
	"github.com/rancher/release-automation/internal/config"
	"github.com/rancher/release-automation/internal/pr"
)

// TestRunCascade_Fixtures walks every subdirectory under
// testdata/runcascade/, drives RunCascade against an in-memory fake GitHub
// client and a recording fake bumper, and compares the resulting cascade
// plan (extracted from the persisted issue body) plus the captured
// side-effects against the case's expected.yaml.
//
// Add a new scenario by creating a new directory with three files:
// dependencies.yaml (config), state.yaml (initial fake state + trigger),
// expected.yaml (expected plan + side-effects). No new Go required.
func TestRunCascade_Fixtures(t *testing.T) {
	root := "testdata/runcascade"
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			runFixture(t, filepath.Join(root, e.Name()))
		})
	}
}

func TestFillTagPromptHints(t *testing.T) {
	const (
		unknownMinor = "unknown (version determined independant from rancher)"
		urlFmt       = "https://github.com/%s/actions/workflows/cut-release.yaml"
	)
	mustTable := func(rows ...string) *config.VersionTable {
		t.Helper()
		md := "| Branch | Minor |\n|---|---|\n"
		for _, r := range rows {
			md += r + "\n"
		}
		tbl, err := config.ParseVersionTable(md)
		if err != nil {
			t.Fatalf("parse table: %v", err)
		}
		return tbl
	}
	leafTable := mustTable("| release/v2.12 | v2.12 |")
	webhookTable := mustTable("| main | v0.11 |", "| release/v0.10 | v0.10 |")

	cfg := &config.Config{Repos: map[string]config.Repo{
		"rancher":         {Kind: config.KindLeaf, Repo: "rancher/rancher"},
		"webhook":         {Kind: config.KindPaired, Repo: "rancher/webhook", NextTagStrategy: config.NextTagRC},
		"webhook-unrc":    {Kind: config.KindPaired, Repo: "rancher/webhook-unrc", NextTagStrategy: config.NextTagUnRC},
		"wrangler":        {Kind: config.KindIndependent, Repo: "rancher/wrangler"},
		"no-tag-access":   {Kind: config.KindPaired, Repo: "rancher/no-tag-access"},
		"missing-gh-repo": {Kind: config.KindPaired},
	}}
	ghRepos := func() map[string]*fakeRepoState {
		return map[string]*fakeRepoState{
			"rancher/rancher":      {Tags: []string{"v2.12.1", "v2.12.0", "v2.11.5"}},
			"rancher/webhook":      {Tags: []string{"v0.11.0-rc.5", "v0.10.3"}},
			"rancher/webhook-unrc": {Tags: []string{"v0.11.0-rc.5", "v0.10.3"}},
			"rancher/wrangler":     {Tags: []string{"v0.7.5"}},
		}
	}

	type tag = cascade.TagPrompt
	tests := []struct {
		name            string
		leafTable       *config.VersionTable
		dependentTables map[string]*config.VersionTable
		stages          []cascade.Stage
		want            []cascade.Stage
		wantErr         string
	}{
		{
			name:      "leaf repo uses leaf table with default patch strategy",
			leafTable: leafTable,
			stages:    []cascade.Stage{{Tags: []tag{{Repo: "rancher", Branch: "release/v2.12"}}}},
			want: []cascade.Stage{{Tags: []tag{
				{Repo: "rancher", Branch: "release/v2.12", Expected: "v2.12.2", WorkflowURL: fmt.Sprintf(urlFmt, "rancher/rancher")},
			}}},
		},
		{
			name:            "paired repo uses its own table with rc strategy",
			dependentTables: map[string]*config.VersionTable{"webhook": webhookTable},
			stages:          []cascade.Stage{{Tags: []tag{{Repo: "webhook", Branch: "main"}}}},
			want: []cascade.Stage{{Tags: []tag{
				{Repo: "webhook", Branch: "main", Expected: "v0.11.0-rc.6", WorkflowURL: fmt.Sprintf(urlFmt, "rancher/webhook")},
			}}},
		},
		{
			name:            "paired repo table wins over leaf table",
			leafTable:       mustTable("| main | v9.9 |"),
			dependentTables: map[string]*config.VersionTable{"webhook": webhookTable},
			stages:          []cascade.Stage{{Tags: []tag{{Repo: "webhook", Branch: "release/v0.10"}}}},
			want: []cascade.Stage{{Tags: []tag{
				{Repo: "webhook", Branch: "release/v0.10", Expected: "v0.10.4-rc.1", WorkflowURL: fmt.Sprintf(urlFmt, "rancher/webhook")},
			}}},
		},
		{
			name:            "unrc strategy drops the rc suffix",
			dependentTables: map[string]*config.VersionTable{"webhook-unrc": webhookTable},
			stages:          []cascade.Stage{{Tags: []tag{{Repo: "webhook-unrc", Branch: "main"}}}},
			want: []cascade.Stage{{Tags: []tag{
				{Repo: "webhook-unrc", Branch: "main", Expected: "v0.11.0", WorkflowURL: fmt.Sprintf(urlFmt, "rancher/webhook-unrc")},
			}}},
		},
		{
			name:   "independent repo ignores version tables",
			stages: []cascade.Stage{{Tags: []tag{{Repo: "wrangler", Branch: "main"}}}},
			want: []cascade.Stage{{Tags: []tag{
				{Repo: "wrangler", Branch: "main", Expected: unknownMinor + ".0", WorkflowURL: fmt.Sprintf(urlFmt, "rancher/wrangler")},
			}}},
		},
		{
			name:            "branch missing from table leaves Expected blank but sets URL",
			dependentTables: map[string]*config.VersionTable{"webhook": webhookTable},
			stages:          []cascade.Stage{{Tags: []tag{{Repo: "webhook", Branch: "release/v0.1"}}}},
			want: []cascade.Stage{{Tags: []tag{
				{Repo: "webhook", Branch: "release/v0.1", WorkflowURL: fmt.Sprintf(urlFmt, "rancher/webhook")},
			}}},
		},
		{
			name:   "no tables available leaves Expected blank",
			stages: []cascade.Stage{{Tags: []tag{{Repo: "rancher", Branch: "release/v2.12"}}}},
			want: []cascade.Stage{{Tags: []tag{
				{Repo: "rancher", Branch: "release/v2.12", WorkflowURL: fmt.Sprintf(urlFmt, "rancher/rancher")},
			}}},
		},
		{
			name:      "repo not in config is left untouched",
			leafTable: leafTable,
			stages:    []cascade.Stage{{Tags: []tag{{Repo: "unknown", Branch: "main", Expected: "keep"}}}},
			want:      []cascade.Stage{{Tags: []tag{{Repo: "unknown", Branch: "main", Expected: "keep"}}}},
		},
		{
			name:            "release listing failure is tolerated",
			dependentTables: map[string]*config.VersionTable{"no-tag-access": webhookTable},
			stages:          []cascade.Stage{{Tags: []tag{{Repo: "no-tag-access", Branch: "main"}}}},
			want: []cascade.Stage{{Tags: []tag{
				{Repo: "no-tag-access", Branch: "main", WorkflowURL: fmt.Sprintf(urlFmt, "rancher/no-tag-access")},
			}}},
		},
		{
			name:    "repo without GitHub identity errors",
			stages:  []cascade.Stage{{Tags: []tag{{Repo: "missing-gh-repo", Branch: "main"}}}},
			wantErr: "repo missing-gh-repo",
		},
		{
			name:            "multiple stages and tags are all filled",
			leafTable:       leafTable,
			dependentTables: map[string]*config.VersionTable{"webhook": webhookTable},
			stages: []cascade.Stage{
				{Layer: 0, Tags: []tag{{Repo: "webhook", Branch: "main"}, {Repo: "wrangler", Branch: "main"}}},
				{Layer: 1, Tags: []tag{{Repo: "rancher", Branch: "release/v2.12"}}},
				{Layer: 2},
			},
			want: []cascade.Stage{
				{Layer: 0, Tags: []tag{
					{Repo: "webhook", Branch: "main", Expected: "v0.11.0-rc.6", WorkflowURL: fmt.Sprintf(urlFmt, "rancher/webhook")},
					{Repo: "wrangler", Branch: "main", Expected: unknownMinor + ".0", WorkflowURL: fmt.Sprintf(urlFmt, "rancher/wrangler")},
				}},
				{Layer: 1, Tags: []tag{
					{Repo: "rancher", Branch: "release/v2.12", Expected: "v2.12.2", WorkflowURL: fmt.Sprintf(urlFmt, "rancher/rancher")},
				}},
				{Layer: 2},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newWithDeps(testConfigName, cfg, Settings{}, newFakeGH(ghRepos()), nil)
			err := r.fillTagPromptHints(context.Background(), tt.stages, tt.leafTable, tt.dependentTables)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("fillTagPromptHints: %v", err)
			}
			if !reflect.DeepEqual(tt.stages, tt.want) {
				t.Errorf("stages mismatch:\n  got:  %+v\n  want: %+v", tt.stages, tt.want)
			}
		})
	}
}

func runFixture(t *testing.T, dir string) {
	t.Helper()
	cfg := loadConfigFixture(t, filepath.Join(dir, "dependencies.yaml"))
	state := loadStateFixture(t, filepath.Join(dir, "state.yaml"))
	expected := loadExpectedFixture(t, filepath.Join(dir, "expected.yaml"))

	for repo, mods := range state.Modules {
		if cfg.Modules == nil {
			cfg.Modules = map[string][]string{}
		}
		cfg.Modules[repo] = mods
	}

	gh := newFakeGH(state.repoStates())
	bumper := newFakeBumper(gh)

	settings := Settings{
		AutomationRepo: state.AutomationRepo,
		Tokens:         map[string]string{state.AutomationRepo: "fake-token"},
		GitHubActor:    state.GitHubActor,
	}
	r := newWithDeps(testConfigName, cfg, settings, gh, bumper)

	if err := r.RunCascade(context.Background(), state.Trigger.LeafBranch, state.Trigger.Independents); err != nil {
		t.Fatalf("RunCascade: %v", err)
	}

	issues := gh.snapshotIssues()
	if len(issues) != 1 {
		t.Fatalf("want exactly 1 tracker issue, got %d: %+v", len(issues), issues)
	}
	tracker := issues[0]

	persisted, err := cascade.ExtractState(tracker.Body)
	if err != nil {
		t.Fatalf("extract cascade state: %v", err)
	}

	assertSources(t, expected.Sources, persisted.Sources)
	assertStages(t, expected.Stages, persisted.Stages)
	assertIssue(t, expected.Issue, tracker)
	assertPRsOpened(t, expected.PRsOpened, bumper.snapshotCalls())
}

// --- fixture types ----------------------------------------------------------

type stateFixture struct {
	AutomationRepo string                       `yaml:"automation_repo"`
	GitHubActor    string                       `yaml:"github_actor"`
	Trigger        triggerFixture               `yaml:"trigger"`
	Modules        map[string][]string          `yaml:"modules"`
	Repos          map[string]repoStateFixture  `yaml:"repos"`
}

type triggerFixture struct {
	LeafBranch   string            `yaml:"leaf_branch"`
	Independents map[string]string `yaml:"independents"`
}

type repoStateFixture struct {
	GHRepo        string                          `yaml:"gh_repo"`
	DefaultBranch string                          `yaml:"default_branch"`
	Tags          []string                        `yaml:"tags"`
	Branches      map[string]branchStateFixture   `yaml:"branches"`
	TagFiles      map[string]map[string]string    `yaml:"tag_files"`
}

type branchStateFixture struct {
	Files   map[string]string `yaml:"files"`
	AheadOf map[string]int    `yaml:"ahead_of"`
}

type expectedFixture struct {
	Sources   []expectedSource `yaml:"sources"`
	Stages    []expectedStage  `yaml:"stages"`
	Issue     expectedIssue    `yaml:"issue"`
	PRsOpened []expectedPR     `yaml:"prs_opened"`
}

type expectedSource struct {
	Name     string `yaml:"name"`
	Version  string `yaml:"version"`
	Explicit bool   `yaml:"explicit"`
}

type expectedStage struct {
	Layer int            `yaml:"layer"`
	Bumps []expectedBump `yaml:"bumps"`
	Tags  []expectedTag  `yaml:"tags"`
}

type expectedBump struct {
	Repo   string        `yaml:"repo"`
	Branch string        `yaml:"branch"`
	Deps   []expectedDep `yaml:"deps"`
}

type expectedDep struct {
	Dep      string          `yaml:"dep"`
	Module   string          `yaml:"module"`
	Version  string          `yaml:"version"`
	Strategy config.Strategy `yaml:"strategy"`
}

type expectedTag struct {
	Repo     string `yaml:"repo"`
	Branch   string `yaml:"branch"`
	Expected string `yaml:"expected,omitempty"`
}

type expectedIssue struct {
	Title  string   `yaml:"title"`
	Labels []string `yaml:"labels"`
}

type expectedPR struct {
	Repo    string             `yaml:"repo"`
	Base    string             `yaml:"base"`
	Modules []expectedPRModule `yaml:"modules"`
}

type expectedPRModule struct {
	Path    string `yaml:"path"`
	Version string `yaml:"version"`
}

func (s stateFixture) repoStates() map[string]*fakeRepoState {
	out := map[string]*fakeRepoState{}
	for _, repo := range s.Repos {
		state := &fakeRepoState{
			DefaultBranch: repo.DefaultBranch,
			Tags:          append([]string(nil), repo.Tags...),
			Branches:      map[string]fakeBranchState{},
			TagFiles:      map[string]map[string]string{},
		}
		if state.DefaultBranch == "" {
			for name := range repo.Branches {
				state.DefaultBranch = name
				break
			}
		}
		for name, br := range repo.Branches {
			state.Branches[name] = fakeBranchState{
				Files:   copyStrMap(br.Files),
				AheadOf: copyIntMap(br.AheadOf),
			}
		}
		for tag, files := range repo.TagFiles {
			state.TagFiles[tag] = copyStrMap(files)
		}
		out[repo.GHRepo] = state
	}
	return out
}

func copyStrMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyIntMap(in map[string]int) map[string]int {
	if in == nil {
		return nil
	}
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// --- loaders ----------------------------------------------------------------

func loadConfigFixture(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return cfg
}

func loadStateFixture(t *testing.T, path string) stateFixture {
	t.Helper()
	var f stateFixture
	mustReadYAML(t, path, &f)
	return f
}

func loadExpectedFixture(t *testing.T, path string) expectedFixture {
	t.Helper()
	var f expectedFixture
	mustReadYAML(t, path, &f)
	return f
}

func mustReadYAML(t *testing.T, path string, into any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := yaml.Unmarshal(b, into); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

// --- assertions -------------------------------------------------------------

func assertSources(t *testing.T, want []expectedSource, got []cascade.Source) {
	t.Helper()
	var gotNorm []expectedSource
	if len(got) > 0 {
		gotNorm = make([]expectedSource, len(got))
		for i, s := range got {
			gotNorm[i] = expectedSource{Name: s.Name, Version: s.Version, Explicit: s.Explicit}
		}
		sort.Slice(gotNorm, func(i, j int) bool { return gotNorm[i].Name < gotNorm[j].Name })
	}
	var wantNorm []expectedSource
	if len(want) > 0 {
		wantNorm = make([]expectedSource, len(want))
		copy(wantNorm, want)
		sort.Slice(wantNorm, func(i, j int) bool { return wantNorm[i].Name < wantNorm[j].Name })
	}
	if !reflect.DeepEqual(wantNorm, gotNorm) {
		t.Errorf("sources mismatch:\n  got:  %+v\n  want: %+v", gotNorm, wantNorm)
	}
}

func assertStages(t *testing.T, want []expectedStage, got []cascade.Stage) {
	t.Helper()
	gotNorm := normalizeStages(got)
	wantNorm := normalizeExpectedStages(want)
	// `expected` (next-tag hint) is opt-in: a fixture that omits it
	// shouldn't fail when fillTagPromptHints computes one. Clear got's
	// Expected for any tag whose want-pair didn't set it.
	for i := range gotNorm {
		if i >= len(wantNorm) {
			break
		}
		for j := range gotNorm[i].Tags {
			if j >= len(wantNorm[i].Tags) {
				break
			}
			if wantNorm[i].Tags[j].Expected == "" {
				gotNorm[i].Tags[j].Expected = ""
			}
		}
	}
	if !reflect.DeepEqual(wantNorm, gotNorm) {
		t.Errorf("stages mismatch:\n  got:  %s\n  want: %s",
			yamlDump(t, gotNorm), yamlDump(t, wantNorm))
	}
}

func assertIssue(t *testing.T, want expectedIssue, got ghclientIssue) {
	t.Helper()
	if want.Title != "" && got.Title != want.Title {
		t.Errorf("issue title: got %q want %q", got.Title, want.Title)
	}
	if len(want.Labels) > 0 {
		gotLabels := append([]string(nil), got.Labels...)
		sort.Strings(gotLabels)
		wantLabels := append([]string(nil), want.Labels...)
		sort.Strings(wantLabels)
		if !reflect.DeepEqual(gotLabels, wantLabels) {
			t.Errorf("issue labels:\n  got:  %v\n  want: %v", gotLabels, wantLabels)
		}
	}
}

func assertPRsOpened(t *testing.T, want []expectedPR, got []pr.Request) {
	t.Helper()
	var gotNorm []expectedPR
	if len(got) > 0 {
		gotNorm = make([]expectedPR, len(got))
		for i, c := range got {
			var mods []expectedPRModule
			if len(c.Modules) > 0 {
				mods = make([]expectedPRModule, len(c.Modules))
				for j, m := range c.Modules {
					mods[j] = expectedPRModule{Path: m.Path, Version: m.Version}
				}
				sort.Slice(mods, func(a, b int) bool { return mods[a].Path < mods[b].Path })
			}
			gotNorm[i] = expectedPR{Repo: c.Repo, Base: c.BaseBranch, Modules: mods}
		}
		sort.Slice(gotNorm, func(i, j int) bool {
			if gotNorm[i].Repo != gotNorm[j].Repo {
				return gotNorm[i].Repo < gotNorm[j].Repo
			}
			return gotNorm[i].Base < gotNorm[j].Base
		})
	}
	var wantNorm []expectedPR
	if len(want) > 0 {
		wantNorm = make([]expectedPR, len(want))
		for i, p := range want {
			var mods []expectedPRModule
			if len(p.Modules) > 0 {
				mods = make([]expectedPRModule, len(p.Modules))
				copy(mods, p.Modules)
				sort.Slice(mods, func(a, b int) bool { return mods[a].Path < mods[b].Path })
			}
			wantNorm[i] = expectedPR{Repo: p.Repo, Base: p.Base, Modules: mods}
		}
		sort.Slice(wantNorm, func(i, j int) bool {
			if wantNorm[i].Repo != wantNorm[j].Repo {
				return wantNorm[i].Repo < wantNorm[j].Repo
			}
			return wantNorm[i].Base < wantNorm[j].Base
		})
	}
	if !reflect.DeepEqual(wantNorm, gotNorm) {
		t.Errorf("PRs opened mismatch:\n  got:  %s\n  want: %s",
			yamlDump(t, gotNorm), yamlDump(t, wantNorm))
	}
}

// ghclientIssue is a tiny alias to keep the assertion signature short.
type ghclientIssue = struct {
	Number int
	Title  string
	Body   string
	State  string
	Labels []string
	URL    string
}

func normalizeStages(stages []cascade.Stage) []expectedStage {
	out := make([]expectedStage, len(stages))
	for i, st := range stages {
		var bumps []expectedBump
		if len(st.Bumps) > 0 {
			bumps = make([]expectedBump, len(st.Bumps))
			for j, bp := range st.Bumps {
				var deps []expectedDep
				if len(bp.Deps) > 0 {
					deps = make([]expectedDep, len(bp.Deps))
					for k, d := range bp.Deps {
						deps[k] = expectedDep{Dep: d.Dep, Module: d.Module, Version: d.Version, Strategy: d.Strategy}
					}
					sort.Slice(deps, func(a, b int) bool { return deps[a].Dep < deps[b].Dep })
				}
				bumps[j] = expectedBump{Repo: bp.Repo, Branch: bp.Branch, Deps: deps}
			}
			sort.Slice(bumps, func(a, b int) bool {
				if bumps[a].Repo != bumps[b].Repo {
					return bumps[a].Repo < bumps[b].Repo
				}
				return bumps[a].Branch < bumps[b].Branch
			})
		}
		var tags []expectedTag
		if len(st.Tags) > 0 {
			tags = make([]expectedTag, len(st.Tags))
			for j, tg := range st.Tags {
				tags[j] = expectedTag{Repo: tg.Repo, Branch: tg.Branch, Expected: tg.Expected}
			}
			sort.Slice(tags, func(a, b int) bool {
				if tags[a].Repo != tags[b].Repo {
					return tags[a].Repo < tags[b].Repo
				}
				return tags[a].Branch < tags[b].Branch
			})
		}
		out[i] = expectedStage{Layer: st.Layer, Bumps: bumps, Tags: tags}
	}
	return out
}

func normalizeExpectedStages(stages []expectedStage) []expectedStage {
	out := make([]expectedStage, len(stages))
	for i, st := range stages {
		var bumps []expectedBump
		if len(st.Bumps) > 0 {
			bumps = make([]expectedBump, len(st.Bumps))
			for j, bp := range st.Bumps {
				var deps []expectedDep
				if len(bp.Deps) > 0 {
					deps = make([]expectedDep, len(bp.Deps))
					for k, d := range bp.Deps {
						deps[k] = d
						if deps[k].Strategy == "" {
							deps[k].Strategy = config.StrategyGoGet
						}
					}
					sort.Slice(deps, func(a, b int) bool { return deps[a].Dep < deps[b].Dep })
				}
				bumps[j] = expectedBump{Repo: bp.Repo, Branch: bp.Branch, Deps: deps}
			}
			sort.Slice(bumps, func(a, b int) bool {
				if bumps[a].Repo != bumps[b].Repo {
					return bumps[a].Repo < bumps[b].Repo
				}
				return bumps[a].Branch < bumps[b].Branch
			})
		}
		var tags []expectedTag
		if len(st.Tags) > 0 {
			tags = make([]expectedTag, len(st.Tags))
			for j, tg := range st.Tags {
				tags[j] = tg
			}
			sort.Slice(tags, func(a, b int) bool {
				if tags[a].Repo != tags[b].Repo {
					return tags[a].Repo < tags[b].Repo
				}
				return tags[a].Branch < tags[b].Branch
			})
		}
		out[i] = expectedStage{Layer: st.Layer, Bumps: bumps, Tags: tags}
	}
	return out
}

func yamlDump(t *testing.T, v any) string {
	t.Helper()
	b, err := yaml.Marshal(v)
	if err != nil {
		t.Fatalf("yaml dump: %v", err)
	}
	return "\n" + string(b)
}
