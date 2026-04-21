package reconcile

import (
	"strings"
	"testing"
	"time"

	"github.com/rancher/release-automation/internal/dashboard"
	ghclient "github.com/rancher/release-automation/internal/github"
)

func TestItemsForLeafBranch(t *testing.T) {
	bodyWith := func(state string) string {
		return "x\n<!-- bump-op-state v1\n" +
			"targets:\n" +
			"  - {repo: rancher, branch: main, pr: 1, state: " + state + "}\n" +
			"  - {repo: rancher, branch: release/v2.13, pr: 2, state: open}\n" +
			"  - {repo: steve, branch: main, pr: 3, state: merged}\n" +
			"-->\n"
	}
	open := []*ghclient.Issue{
		{Number: 100, Title: "[bump] wrangler v0.5.1", URL: "https://x/100", Labels: []string{"bump-op", "dep:wrangler"}, Body: bodyWith("merged")},
		{Number: 101, Title: "[bump] norman v0.7.5", URL: "https://x/101", Labels: []string{"bump-op", "dep:norman"}, Body: bodyWith("open")},
		{Number: 102, Title: "[bump] lasso v1.0.0", URL: "https://x/102", Labels: []string{"bump-op", "dep:lasso"}, Body: "<!-- bump-op-state v1\ntargets:\n  - {repo: steve, branch: main, pr: 9, state: open}\n-->\n"},
	}

	items, err := itemsForLeafBranch("rancher", "main", open)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("want 2 items targeting rancher main, got %d: %+v", len(items), items)
	}
	if items[0].Dep != "norman" || items[1].Dep != "wrangler" {
		t.Errorf("sort order wrong: %+v", items)
	}
	if items[0].Merged != 1 || items[0].Total != 3 {
		t.Errorf("norman counts wrong: %+v", items[0])
	}
	if items[1].Merged != 2 || items[1].Total != 3 {
		t.Errorf("wrangler counts wrong: %+v", items[1])
	}
}

func TestRenderDashboardEmpty(t *testing.T) {
	body := dashboard.Render("rancher", "main", nil, time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC))
	if !strings.Contains(body, "_no in-flight bumps_") {
		t.Errorf("empty render missing placeholder: %s", body)
	}
	if !strings.Contains(body, "Last updated: 2026-04-20T12:00:00Z") {
		t.Errorf("empty render missing timestamp: %s", body)
	}
}

func TestRenderDashboardItem(t *testing.T) {
	items := []dashboard.Item{
		{Dep: "wrangler", Version: "v0.5.1", TrackerNum: 100, TrackerURL: "https://x/100", Total: 4, Merged: 2},
	}
	body := dashboard.Render("rancher", "main", items, time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC))
	if !strings.Contains(body, "[bump] wrangler v0.5.1 [#100](https://x/100) — 2/4 PRs merged") {
		t.Errorf("item line missing or malformed: %s", body)
	}
}
