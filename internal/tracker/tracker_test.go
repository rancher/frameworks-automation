package tracker

import (
	"strings"
	"testing"
	"time"
)

func TestRoundTripState(t *testing.T) {
	body := "## Trigger\nsteve v0.7.5 released\n"
	in := Persistent{
		SlackThreadTS: "1729451234.001900",
		Targets: []Target{
			{Repo: "rancher", Branch: "main", PR: 1234, State: "open"},
			{Repo: "rancher", Branch: "release/v2.13", PR: 1235, State: "open"},
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
	if len(got.Targets) != 2 || got.Targets[0].PR != 1234 || got.Targets[1].Branch != "release/v2.13" {
		t.Errorf("targets: got %+v", got.Targets)
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
	if got.SlackThreadTS != "" || len(got.Targets) != 0 {
		t.Errorf("expected zero state, got %+v", got)
	}
}

func TestRender_BodyContainsTargetsAndState(t *testing.T) {
	op := Op{
		Dep:     "steve",
		Version: "v0.7.5",
		Targets: []Target{
			{Repo: "rancher", Branch: "release/v2.13", PR: 1234, PRURL: "https://github.com/rancher/rancher/pull/1234", State: "open"},
			{Repo: "rancher", Branch: "main"},
		},
	}
	body, err := Render(op, time.Date(2026, 4, 21, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(body, "[bump]") || strings.Contains(body, "[bump]") && !strings.Contains(body, "steve v0.7.5") {
		// Title isn't in the body — only check trigger line.
		if !strings.Contains(body, "steve v0.7.5 released") {
			t.Errorf("body missing trigger: %s", body)
		}
	}
	if !strings.Contains(body, "[#1234](") || !strings.Contains(body, "(open)") {
		t.Errorf("body missing linked PR ref: %s", body)
	}
	if !strings.Contains(body, "_pending_") {
		t.Errorf("body missing pending placeholder: %s", body)
	}
	if !strings.Contains(body, "Last reconciled: 2026-04-21T10:00:00Z") {
		t.Errorf("body missing reconciled timestamp: %s", body)
	}
	st, err := ExtractState(body)
	if err != nil {
		t.Fatalf("extract from rendered: %v", err)
	}
	if len(st.Targets) != 2 {
		t.Errorf("expected 2 targets in state, got %d", len(st.Targets))
	}
}

func TestMergeState_PreservesOpTargetsAndOverlaysPR(t *testing.T) {
	op := Op{
		Dep:     "steve",
		Version: "v0.7.5",
		Targets: []Target{
			{Repo: "rancher", Branch: "main"},                  // newly added
			{Repo: "rancher", Branch: "release/v2.13"},         // existing
		},
	}
	stored := Persistent{
		Targets: []Target{
			{Repo: "rancher", Branch: "release/v2.13", PR: 1235, State: "approved"},
		},
	}
	mergeState(&op, stored)
	if op.Targets[0].PR != 0 {
		t.Errorf("new target should have no PR, got %+v", op.Targets[0])
	}
	if op.Targets[1].PR != 1235 || op.Targets[1].State != "approved" {
		t.Errorf("existing target not merged: %+v", op.Targets[1])
	}
}

func TestRenderRef(t *testing.T) {
	cases := []struct {
		name string
		t    Target
		want string
	}{
		{"no PR yet", Target{}, "_pending_"},
		{"open with URL", Target{PR: 42, PRURL: "https://github.com/rancher/steve/pull/42", State: "open"},
			"[#42](https://github.com/rancher/steve/pull/42) (open)"},
		{"empty state defaults to open", Target{PR: 42, PRURL: "https://x/pull/42"},
			"[#42](https://x/pull/42) (open)"},
		{"ci-failing links to checks tab", Target{PR: 88, PRURL: "https://github.com/rancher/apiserver/pull/88", State: "ci-failing"},
			"[#88](https://github.com/rancher/apiserver/pull/88) ([ci-failing](https://github.com/rancher/apiserver/pull/88/checks))"},
		{"merged terminal", Target{PR: 15, PRURL: "https://x/pull/15", State: "merged"},
			"[#15](https://x/pull/15) (merged)"},
		{"missing URL falls back to plain text", Target{PR: 7, State: "open"},
			"#7 (open)"},
	}
	for _, c := range cases {
		if got := renderRef(c.t); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestParseVersionFromTitle(t *testing.T) {
	cases := []struct {
		title, dep, want string
	}{
		{"[bump] wrangler v0.5.1", "wrangler", "v0.5.1"},
		{"[bump] steve v0.7.5-rc1", "steve", "v0.7.5-rc1"},
		{"[bump] wrangler v0.5.1", "steve", ""},                     // wrong dep
		{"random title", "wrangler", ""},                            // wrong shape
		{"[bump] wranglerv0.5.1", "wrangler", ""},                   // missing space
		{Title("apiserver", "v0.10.0"), "apiserver", "v0.10.0"},     // round-trip
	}
	for _, c := range cases {
		if got := ParseVersionFromTitle(c.title, c.dep); got != c.want {
			t.Errorf("ParseVersionFromTitle(%q, %q) = %q, want %q", c.title, c.dep, got, c.want)
		}
	}
}

func TestLabelsHasNoVersion(t *testing.T) {
	got := Labels("wrangler")
	for _, l := range got {
		if strings.HasPrefix(l, "version:") {
			t.Errorf("Labels should not include a version: label, got %v", got)
		}
	}
}

func TestRepoFromPRURL(t *testing.T) {
	got, err := repoFromPRURL("https://github.com/rancher/rancher/pull/1234")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "rancher/rancher" {
		t.Errorf("got %q want rancher/rancher", got)
	}
	if _, err := repoFromPRURL("not a url"); err == nil {
		t.Error("expected err for non-url")
	}
}
