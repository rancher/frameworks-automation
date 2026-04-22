package reconcile

import (
	"testing"

	"github.com/rancher/release-automation/internal/cascade"
)

func TestAllBumpsMerged(t *testing.T) {
	cases := []struct {
		name string
		st   cascade.Stage
		want bool
	}{
		{"empty", cascade.Stage{}, true},
		{"all merged", cascade.Stage{Bumps: []cascade.Bump{{State: "merged"}, {State: "merged"}}}, true},
		{"one open", cascade.Stage{Bumps: []cascade.Bump{{State: "merged"}, {State: "open"}}}, false},
		{"one closed (not merged)", cascade.Stage{Bumps: []cascade.Bump{{State: "merged"}, {State: "closed"}}}, false},
	}
	for _, c := range cases {
		if got := allBumpsMerged(c.st); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestAllTagsSatisfied(t *testing.T) {
	cases := []struct {
		name string
		st   cascade.Stage
		want bool
	}{
		{"no tags (final)", cascade.Stage{}, true},
		{"all tagged", cascade.Stage{Tags: []cascade.TagPrompt{{Tagged: true, Version: "v1"}, {Tagged: true, Version: "v2"}}}, true},
		{"one untagged", cascade.Stage{Tags: []cascade.TagPrompt{{Tagged: true, Version: "v1"}, {Tagged: false}}}, false},
		{"tagged but no version", cascade.Stage{Tags: []cascade.TagPrompt{{Tagged: true}}}, false},
	}
	for _, c := range cases {
		if got := allTagsSatisfied(c.st); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestCascadeComplete(t *testing.T) {
	mid := cascade.Op{
		CurrentStage: 0,
		Stages: []cascade.Stage{
			{Bumps: []cascade.Bump{{State: "merged"}}},
			{Bumps: []cascade.Bump{{State: "open"}}},
		},
	}
	if cascadeComplete(mid) {
		t.Error("not on final stage — should not be complete")
	}
	finalNotMerged := cascade.Op{
		CurrentStage: 1,
		Stages: []cascade.Stage{
			{Bumps: []cascade.Bump{{State: "merged"}}},
			{Bumps: []cascade.Bump{{State: "open"}}},
		},
	}
	if cascadeComplete(finalNotMerged) {
		t.Error("final stage open — should not be complete")
	}
	done := cascade.Op{
		CurrentStage: 1,
		Stages: []cascade.Stage{
			{Bumps: []cascade.Bump{{State: "merged"}}},
			{Bumps: []cascade.Bump{{State: "merged"}}},
		},
	}
	if !cascadeComplete(done) {
		t.Error("final stage merged — should be complete")
	}
}

func TestLeafFromLabels(t *testing.T) {
	cases := []struct {
		name              string
		labels            []string
		wantRepo, wantBr  string
	}{
		{"main", []string{"cascade-op", "dep:wrangler", "leaf:rancher:main"}, "rancher", "main"},
		{"release branch", []string{"leaf:rancher:release/v2.13"}, "rancher", "release/v2.13"},
		{"missing", []string{"cascade-op", "dep:wrangler"}, "", ""},
	}
	for _, c := range cases {
		repo, br := leafFromLabels(c.labels)
		if repo != c.wantRepo || br != c.wantBr {
			t.Errorf("%s: got (%q,%q) want (%q,%q)", c.name, repo, br, c.wantRepo, c.wantBr)
		}
	}
}

func TestCascadeBumpBranchName(t *testing.T) {
	got := cascadeBumpBranchName("wrangler", "v0.5.1", "release/v2.13", "wrangler", "v0.5.1")
	want := "automation/cascade-wrangler-v0.5.1-leaf-release-v2.13-bump-wrangler-v0.5.1"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}
