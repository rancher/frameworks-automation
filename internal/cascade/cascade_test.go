package cascade

import (
	"strings"
	"testing"
	"time"
)

func TestRoundTripState(t *testing.T) {
	body := "## Cascade\nwrangler v0.5.1 → rancher main\n"
	in := Persistent{
		SlackThreadTS: "1729451234.001900",
		CurrentStage:  1,
		Stages: []Stage{
			{Layer: 1, Bumps: []Bump{{Repo: "steve", Branch: "main", Dep: "wrangler", Module: "github.com/x/wrangler", Version: "v0.5.1", PR: 42, State: "merged"}}, Tags: []TagPrompt{{Repo: "steve", Branch: "main", Version: "v0.7.6", Tagged: true}}},
			{Layer: 2, Bumps: []Bump{{Repo: "rancher", Branch: "main", Dep: "steve", Module: "github.com/x/steve", Version: "v0.7.6", PR: 99, State: "open"}}},
		},
	}
	updated, err := EmbedState(body, in)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	got, err := ExtractState(updated)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got.SlackThreadTS != in.SlackThreadTS {
		t.Errorf("ts: got %q want %q", got.SlackThreadTS, in.SlackThreadTS)
	}
	if got.CurrentStage != 1 {
		t.Errorf("current_stage: got %d want 1", got.CurrentStage)
	}
	if len(got.Stages) != 2 || got.Stages[0].Bumps[0].PR != 42 || got.Stages[1].Bumps[0].Dep != "steve" {
		t.Errorf("stages: got %+v", got.Stages)
	}
	if !got.Stages[0].Tags[0].Tagged || got.Stages[0].Tags[0].Version != "v0.7.6" {
		t.Errorf("tag prompt: got %+v", got.Stages[0].Tags[0])
	}
}

func TestEmbedReplacesExisting(t *testing.T) {
	body := "header\n"
	first, _ := EmbedState(body, Persistent{SlackThreadTS: "111"})
	second, err := EmbedState(first, Persistent{SlackThreadTS: "222"})
	if err != nil {
		t.Fatalf("embed second: %v", err)
	}
	got, _ := ExtractState(second)
	if got.SlackThreadTS != "222" {
		t.Errorf("ts after replace: got %q want 222", got.SlackThreadTS)
	}
}

func TestExtractMissingBlock(t *testing.T) {
	got, err := ExtractState("just a body\n")
	if err != nil {
		t.Fatalf("expected no error for missing block, got: %v", err)
	}
	if got.SlackThreadTS != "" || len(got.Stages) != 0 {
		t.Errorf("expected zero state, got %+v", got)
	}
}

func TestRender_BodyContainsStagesAndState(t *testing.T) {
	op := Op{
		Dep:        "wrangler",
		Version:    "v0.5.1",
		LeafRepo:   "rancher",
		LeafBranch: "main",
		CurrentStage: 0,
		Stages: []Stage{
			{Layer: 1,
				Bumps: []Bump{{Repo: "steve", Branch: "main", Dep: "wrangler", Module: "github.com/x/wrangler", Version: "v0.5.1", PR: 42, PRURL: "https://github.com/x/y/pull/42", State: "open"}},
				Tags:  []TagPrompt{{Repo: "steve", Branch: "main"}},
			},
			{Layer: 2,
				Bumps: []Bump{{Repo: "rancher", Branch: "main", Dep: "steve", Module: "github.com/x/steve"}},
			},
		},
	}
	body, err := Render(op, time.Date(2026, 4, 21, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(body, "wrangler v0.5.1 → rancher main") {
		t.Errorf("body missing header line: %s", body)
	}
	if !strings.Contains(body, "Stage 1: bump → tag (current)") {
		t.Errorf("body missing stage 1 marker: %s", body)
	}
	if !strings.Contains(body, "Stage 2: bump (final)") {
		t.Errorf("body missing stage 2 final marker: %s", body)
	}
	if !strings.Contains(body, "[#42](https://github.com/x/y/pull/42) (open)") {
		t.Errorf("body missing linked PR ref: %s", body)
	}
	if !strings.Contains(body, "(steve@_pending_)") {
		t.Errorf("body missing pending version placeholder: %s", body)
	}
	if !strings.Contains(body, "Last reconciled: 2026-04-21T10:00:00Z") {
		t.Errorf("body missing reconciled timestamp: %s", body)
	}
	st, err := ExtractState(body)
	if err != nil {
		t.Fatalf("extract from rendered: %v", err)
	}
	if len(st.Stages) != 2 {
		t.Errorf("expected 2 stages in state, got %d", len(st.Stages))
	}
}

func TestRenderTagRef(t *testing.T) {
	cases := []struct {
		name string
		t    TagPrompt
		want string
	}{
		{"pending no hints", TagPrompt{}, "_pending_"},
		{"pending with expected only", TagPrompt{Expected: "v0.7.6"}, "expected v0.7.6"},
		{"pending with url only", TagPrompt{WorkflowURL: "https://x/actions"}, "_pending_ ([run Release workflow](https://x/actions))"},
		{"pending with expected + url", TagPrompt{Expected: "v0.7.6", WorkflowURL: "https://x/actions"},
			"expected v0.7.6 ([run Release workflow](https://x/actions))"},
		{"tagged collapses to version", TagPrompt{Tagged: true, Version: "v0.7.6", Expected: "v0.7.6", WorkflowURL: "https://x"}, "v0.7.6"},
		{"tagged but no version", TagPrompt{Tagged: true}, "_tagged_"},
	}
	for _, c := range cases {
		if got := renderTagRef(c.t); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestRenderBumpRef(t *testing.T) {
	cases := []struct {
		name string
		b    Bump
		want string
	}{
		{"no PR yet", Bump{}, "_pending_"},
		{"open with URL", Bump{PR: 42, PRURL: "https://x/pull/42", State: "open"}, "[#42](https://x/pull/42) (open)"},
		{"empty state defaults to open", Bump{PR: 42, PRURL: "https://x/pull/42"}, "[#42](https://x/pull/42) (open)"},
		{"ci-failing links to checks", Bump{PR: 88, PRURL: "https://x/pull/88", State: "ci-failing"}, "[#88](https://x/pull/88) ([ci-failing](https://x/pull/88/checks))"},
		{"merged terminal", Bump{PR: 15, PRURL: "https://x/pull/15", State: "merged"}, "[#15](https://x/pull/15) (merged)"},
		{"missing URL falls back to plain", Bump{PR: 7, State: "open"}, "#7 (open)"},
	}
	for _, c := range cases {
		if got := renderBumpRef(c.b); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestParseVersionFromTitle(t *testing.T) {
	cases := []struct {
		title, dep, want string
	}{
		{"[cascade] wrangler v0.5.1 → rancher main", "wrangler", "v0.5.1"},
		{"[cascade] steve v0.7.5-rc1 → rancher release/v2.13", "steve", "v0.7.5-rc1"},
		{"[cascade] wrangler v0.5.1 → rancher main", "steve", ""},
		{"[bump] wrangler v0.5.1 → rancher main", "wrangler", ""}, // bump tracker, not cascade
		{"random title", "wrangler", ""},
		{Title("apiserver", "v0.10.0", "rancher", "release/v2.13"), "apiserver", "v0.10.0"},
	}
	for _, c := range cases {
		if got := ParseVersionFromTitle(c.title, c.dep); got != c.want {
			t.Errorf("ParseVersionFromTitle(%q, %q) = %q, want %q", c.title, c.dep, got, c.want)
		}
	}
}

func TestLabelsAndTitleAndLeafLabel(t *testing.T) {
	if got, want := Title("wrangler", "v0.5.1", "rancher", "main"), "[cascade] wrangler v0.5.1 → rancher main"; got != want {
		t.Errorf("Title: got %q want %q", got, want)
	}
	got := Labels("wrangler", "rancher", "release/v2.13")
	want := []string{"cascade-op", "dep:wrangler", "leaf:rancher:release/v2.13"}
	if len(got) != len(want) {
		t.Fatalf("Labels len: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Labels[%d]: got %q want %q", i, got[i], want[i])
		}
	}
	if got, want := LeafLabel("rancher", "release/v2.13"), "leaf:rancher:release/v2.13"; got != want {
		t.Errorf("LeafLabel: got %q want %q", got, want)
	}
}
