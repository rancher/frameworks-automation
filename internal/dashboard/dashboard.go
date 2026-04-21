// Package dashboard renders the per-leaf-branch "in-flight bumps" issue.
// One long-lived issue per (leaf-repo, branch) — body is fully regenerated
// each tick from the open bump-op trackers, so there's no state to persist.
package dashboard

import (
	"fmt"
	"strings"
	"time"
)

const (
	LabelDashboard     = "dashboard"
	LabelLeafBranchFmt = "dashboard:%s:%s" // dashboard:rancher:main
)

// Item is one in-flight bump rolled up for this dashboard.
type Item struct {
	Dep        string
	Version    string
	TrackerNum int
	TrackerURL string
	Total      int // count of targets across the whole bump op
	Merged     int // count of those marked "merged"
}

// Title is the canonical issue title for a (leaf, branch) dashboard.
func Title(leaf, branch string) string {
	return fmt.Sprintf("%s %s: in-flight bumps", leaf, branch)
}

// Labels returns the canonical label set for find-or-create lookup. The
// branch-scoped label is unique per dashboard, so listing by it returns
// at most one issue.
func Labels(leaf, branch string) []string {
	return []string{LabelDashboard, fmt.Sprintf(LabelLeafBranchFmt, leaf, branch)}
}

// Render produces the dashboard body. Items should already be sorted by the
// caller for stable output.
func Render(leaf, branch string, items []Item, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s %s — in-flight bumps\n", leaf, branch)
	fmt.Fprintf(&b, "Last updated: %s\n\n", now.UTC().Format(time.RFC3339))
	b.WriteString("## Bump operations\n")
	if len(items) == 0 {
		b.WriteString("_no in-flight bumps_\n")
		return b.String()
	}
	for _, it := range items {
		fmt.Fprintf(&b, "- [bump] %s %s [#%d](%s) — %d/%d PRs merged\n",
			it.Dep, it.Version, it.TrackerNum, it.TrackerURL, it.Merged, it.Total)
	}
	return b.String()
}
