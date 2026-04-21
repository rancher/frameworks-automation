// Package tracker owns the lifecycle of bump-op tracker issues. The issue
// body doubles as the state store: a fenced metadata block holds the
// per-target PR number, last-known PR state, and (later) the Slack thread ts
// for in-thread replies.
//
// Tracker identity is (dep, version, leaf-branch) — one tracker per bump
// landing on a specific leaf branch. Lookup is by labels: `bump-op`,
// `dep:<name>`, `leaf:<leaf-repo>:<leaf-branch>`.
package tracker

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	LabelOp      = "bump-op"
	LabelDepFmt  = "dep:%s"          // e.g. dep:steve
	LabelLeafFmt = "leaf:%s:%s"      // e.g. leaf:rancher:main, leaf:rancher:release/v2.13

	stateOpen  = "<!-- bump-op-state v1"
	stateClose = "-->"
)

// titlePrefixFmt matches Title(): "[bump] {dep} {version} → {leafRepo} {leafBranch}".
// Stable — ParseVersionFromTitle relies on the prefix and the " → " separator.
const (
	titlePrefixFmt = "[bump] %s "
	titleArrow     = " → "
)

// Op is a single bump operation: a (dep, version) landing on one leaf branch
// (e.g. rancher main, rancher release/v2.13). Targets are the per-downstream
// branches that ship against that leaf branch — ordered for stable rendering
// (keep them sorted by Repo,Branch).
type Op struct {
	Dep        string
	Version    string
	LeafRepo   string // e.g. "rancher" — the leaf this bump targets
	LeafBranch string // e.g. "main", "release/v2.13"
	Targets    []Target
}

// Target is one downstream branch. PR is 0 until a bump PR is opened.
// State is the last PR-state we observed (used by pass 2 to diff & post).
// Values: "" (none yet) | "open" | "ci-failing" | "approved" | "merged" | "closed".
type Target struct {
	Repo   string `yaml:"repo"`
	Branch string `yaml:"branch"`
	PR     int    `yaml:"pr,omitempty"`
	PRURL  string `yaml:"pr_url,omitempty"`
	State  string `yaml:"state,omitempty"`
}

// Persistent is what survives between reconciler runs. Lives in the metadata
// block. Keep YAML tags stable: older runs must read newer files (additive
// only, never rename or remove a field).
type Persistent struct {
	SlackThreadTS string   `yaml:"slack_thread_ts,omitempty"`
	Targets       []Target `yaml:"targets"`
	SupersededBy  *int     `yaml:"superseded_by,omitempty"` // tracker issue number
}

// Title is the canonical issue title for an op. The version is parsed back
// out of the title (see ParseVersionFromTitle) — keep this format stable.
//
// Format: "[bump] {dep} {version} → {leafRepo} {leafBranch}". Encoding the
// leaf in the title makes trackers immediately distinguishable in lists
// (you can tell wrangler v0.5.1→main from wrangler v0.5.1→release/v2.13 at
// a glance) and gives ParseVersionFromTitle a stable separator to split on.
func Title(dep, version, leafRepo, leafBranch string) string {
	return fmt.Sprintf(titlePrefixFmt+"%s"+titleArrow+"%s %s", dep, version, leafRepo, leafBranch)
}

// Labels returns the canonical label set for a tracker. The version is NOT a
// label (it would proliferate one new label per release); it lives in the
// title and is parsed by ParseVersionFromTitle. The leaf label IS used so
// that one tracker per (dep, version, leaf-branch) is queryable directly —
// proliferation here is bounded by leaf branches (a handful).
func Labels(dep, leafRepo, leafBranch string) []string {
	return []string{
		LabelOp,
		fmt.Sprintf(LabelDepFmt, dep),
		fmt.Sprintf(LabelLeafFmt, leafRepo, leafBranch),
	}
}

// LeafLabel is the single leaf-axis label, used by the dashboard to query
// trackers landing on a specific (leafRepo, leafBranch) without filtering.
func LeafLabel(leafRepo, leafBranch string) string {
	return fmt.Sprintf(LabelLeafFmt, leafRepo, leafBranch)
}

// ParseVersionFromTitle returns the version embedded in `title` for `dep`,
// or "" if `title` doesn't match the canonical format. Used during
// FindOrCreate (filter candidates returned by the dep-label query) and
// Supersede (compare versions across open trackers for the same dep+leaf).
func ParseVersionFromTitle(title, dep string) string {
	prefix := fmt.Sprintf(titlePrefixFmt, dep)
	if !strings.HasPrefix(title, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(title, prefix)
	i := strings.Index(rest, titleArrow)
	if i < 0 {
		return ""
	}
	return rest[:i]
}

// Render produces the issue body markdown for `op`, with the metadata block
// already embedded. Idempotent: rendering the same Op twice (same time aside)
// produces the same body.
func Render(op Op, now time.Time) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "## Trigger\n%s %s released\n\n", op.Dep, op.Version)

	b.WriteString("## Targets\n")
	if len(op.Targets) == 0 {
		b.WriteString("_no targets — nothing to bump_\n")
	}
	for _, t := range op.Targets {
		check := " "
		if t.State == "merged" {
			check = "x"
		}
		fmt.Fprintf(&b, "- [%s] `%s` `%s` — %s\n", check, t.Repo, t.Branch, renderRef(t))
	}

	fmt.Fprintf(&b, "\n## Status\nLast reconciled: %s\n\n", now.UTC().Format(time.RFC3339))

	body, err := EmbedState(b.String(), Persistent{Targets: op.Targets})
	if err != nil {
		return "", err
	}
	return body, nil
}

func displayState(s string) string {
	if s == "" {
		return "open"
	}
	return s
}

// renderRef builds the per-target trailing markdown: PR autolink + a state
// label that's itself a link when there's a useful destination. Examples:
//
//	_pending_                                       (no PR yet)
//	[#42](url) (open)                               (PR open, no extra link)
//	[#88](url) ([ci-failing](url/checks))           (state -> PR checks tab)
//	[#15](url) (merged)                             (terminal state)
//
// /pull/N/checks works for both GHA and third-party check-runs, so it's a
// useful target without requiring an extra API call.
func renderRef(t Target) string {
	if t.PR == 0 {
		return "_pending_"
	}
	state := displayState(t.State)
	if t.State == "ci-failing" && t.PRURL != "" {
		state = fmt.Sprintf("[%s](%s/checks)", state, t.PRURL)
	}
	if t.PRURL == "" {
		return fmt.Sprintf("#%d (%s)", t.PR, state)
	}
	return fmt.Sprintf("[#%d](%s) (%s)", t.PR, t.PRURL, state)
}

// ExtractState pulls the metadata block out of an issue body. Returns
// zero-value Persistent (no error) if the block is absent — useful for
// trackers created out-of-band that the reconciler is adopting.
func ExtractState(body string) (Persistent, error) {
	start := strings.Index(body, stateOpen)
	if start < 0 {
		return Persistent{}, nil
	}
	rest := body[start+len(stateOpen):]
	end := strings.Index(rest, stateClose)
	if end < 0 {
		return Persistent{}, fmt.Errorf("metadata block missing closing %q", stateClose)
	}
	var s Persistent
	if err := yaml.Unmarshal([]byte(rest[:end]), &s); err != nil {
		return Persistent{}, fmt.Errorf("parse metadata block: %w", err)
	}
	return s, nil
}

// EmbedState replaces (or appends) the metadata block in `body` with `s`.
// Use this when updating an existing issue body to keep the human-readable
// part intact.
func EmbedState(body string, s Persistent) (string, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(s); err != nil {
		return "", fmt.Errorf("encode metadata: %w", err)
	}
	enc.Close()
	block := stateOpen + "\n" + buf.String() + stateClose

	start := strings.Index(body, stateOpen)
	if start < 0 {
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		return body + "\n" + block + "\n", nil
	}
	end := strings.Index(body[start:], stateClose)
	if end < 0 {
		return "", fmt.Errorf("metadata block missing closing %q", stateClose)
	}
	end += start + len(stateClose)
	return body[:start] + block + body[end:], nil
}
