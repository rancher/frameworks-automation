// Package cascade owns the lifecycle of cascade tracker issues. A cascade
// propagates a single (dep, version) up the dependency DAG to a target leaf
// branch, opening bump PRs one layer at a time and prompting a re-tag at
// each intermediate layer so every layer CIs against the new dep before the
// next layer ships.
//
// Cascade is self-contained: it owns its own PRs (separate from the
// per-(dep, version) bump-op trackers) and its own Slack thread. While a
// cascade is active for (dep, leafBranch), the reconciler's auto-dispatch
// path defers cascade-mid tags to the cascade rather than opening regular
// bump-op trackers (see pass1 coordination).
//
// Tracker identity is (dep, version, leaf-branch). Lookup is by labels:
// `cascade-op`, `dep:<name>`, `leaf:<leaf-repo>:<leaf-branch>`. The version
// lives in the title (parsed by ParseVersionFromTitle) — same approach as
// the bump-op tracker, for the same reason (avoid a label per version).
package cascade

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	LabelOp      = "cascade-op"
	LabelDepFmt  = "dep:%s"
	LabelLeafFmt = "leaf:%s:%s"

	stateOpen  = "<!-- cascade-op-state v1"
	stateClose = "-->"
)

// titlePrefixFmt matches Title(): "[cascade] {dep} {version} → {leafRepo} {leafBranch}".
// Stable — ParseVersionFromTitle relies on the prefix and the " → " separator.
const (
	titlePrefixFmt = "[cascade] %s "
	titleArrow     = " → "
)

// Bump is one bump-PR slot inside a cascade stage. There's exactly one Bump
// per (Repo, Branch) per stage — every in-scope dep that needs bumping at
// this layer rides in the same PR (Deps). Bundling avoids sibling-PR conflicts
// on go.mod/go.sum and matches what the cascade-mid tag CIs against: the
// combined post-bump tree.
//
// Within a Bump, each Dep's Version is fixed at cascade creation when known
// (the source dep, in the stage that bumps it directly) or "" until a prior
// stage's tag arrives.
type Bump struct {
	Repo   string    `yaml:"repo"`
	Branch string    `yaml:"branch"`
	Deps   []DepBump `yaml:"deps"`
	PR     int       `yaml:"pr,omitempty"`
	PRURL  string    `yaml:"pr_url,omitempty"`
	State  string    `yaml:"state,omitempty"` // "" | open | ci-failing | approved | merged | closed
}

// DepBump is one (dep, module, version) triple inside a Bump's bundle.
// `Dep` is the config-key name of the dep being bumped (e.g. "wrangler");
// `Module` is its Go module path; `Version` is the target tag, "" until a
// prior stage's tag arrives.
type DepBump struct {
	Dep     string `yaml:"dep"`
	Module  string `yaml:"module"`
	Version string `yaml:"version,omitempty"`
}

// TagPrompt is one tag the cascade waits on at the end of a non-final stage.
// Version is observed (not predicted) — set when the dev runs the per-repo
// Release workflow and the resulting tag-emitted dispatch claims this slot.
//
// Expected is the next-patch suggestion shown alongside the prompt: computed
// at cascade creation by listing the repo's releases and incrementing the
// highest patch matching this branch's minor. Stale if someone tags a
// higher patch outside the cascade flow — the dev sees a hint, not a hard
// constraint (the per-repo Release workflow validates the version anyway).
//
// WorkflowURL points reviewers at the repo's Actions tab so they can run the
// Release workflow without hunting for it.
type TagPrompt struct {
	Repo        string `yaml:"repo"`
	Branch      string `yaml:"branch"`
	Version     string `yaml:"version,omitempty"`
	Tagged      bool   `yaml:"tagged,omitempty"`
	Expected    string `yaml:"expected,omitempty"`
	WorkflowURL string `yaml:"workflow_url,omitempty"`
}

// Stage is one layer of the cascade. Bumps land first; once all merge, Tags
// (if any) become live prompts. The final stage has no Tags.
type Stage struct {
	Layer int         `yaml:"layer"`
	Bumps []Bump      `yaml:"bumps"`
	Tags  []TagPrompt `yaml:"tags,omitempty"`
}

// Op identifies a cascade and its planned stages. CurrentStage is 0-indexed
// into Stages and tracks how far we've advanced.
type Op struct {
	Dep          string
	Version      string
	LeafRepo     string
	LeafBranch   string
	Stages       []Stage
	CurrentStage int
}

// Persistent is what survives between reconciler runs (lives in the metadata
// block). Additive only — older runs must read newer files.
type Persistent struct {
	SlackThreadTS string  `yaml:"slack_thread_ts,omitempty"`
	Stages        []Stage `yaml:"stages"`
	CurrentStage  int     `yaml:"current_stage"`
}

// Title is the canonical issue title for a cascade. Parsed back out by
// ParseVersionFromTitle — keep the format stable.
func Title(dep, version, leafRepo, leafBranch string) string {
	return fmt.Sprintf(titlePrefixFmt+"%s"+titleArrow+"%s %s", dep, version, leafRepo, leafBranch)
}

// Labels returns the canonical label set. Version is not a label (would
// proliferate one per release); it lives in the title.
func Labels(dep, leafRepo, leafBranch string) []string {
	return []string{
		LabelOp,
		fmt.Sprintf(LabelDepFmt, dep),
		fmt.Sprintf(LabelLeafFmt, leafRepo, leafBranch),
	}
}

// LeafLabel is the leaf-axis label used by the dashboard.
func LeafLabel(leafRepo, leafBranch string) string {
	return fmt.Sprintf(LabelLeafFmt, leafRepo, leafBranch)
}

// ParseVersionFromTitle returns the version embedded in `title` for `dep`,
// or "" if `title` doesn't match the canonical cascade format.
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

// Render produces the cascade issue body markdown with embedded state.
// Idempotent (same Op + same time → same body).
func Render(op Op, now time.Time) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "## Cascade\n%s %s → %s %s\n\n", op.Dep, op.Version, op.LeafRepo, op.LeafBranch)

	for i, st := range op.Stages {
		marker := ""
		switch {
		case i < op.CurrentStage:
			marker = " (done)"
		case i == op.CurrentStage:
			marker = " (current)"
		}
		final := i == len(op.Stages)-1
		header := "bump → tag"
		if final {
			header = "bump (final)"
		}
		fmt.Fprintf(&b, "## Stage %d: %s%s\n", st.Layer, header, marker)
		for _, bp := range st.Bumps {
			check := " "
			if bp.State == "merged" {
				check = "x"
			}
			fmt.Fprintf(&b, "- [%s] bump `%s` `%s` — %s (%s)\n", check, bp.Repo, bp.Branch, renderBumpRef(bp), renderDepList(bp.Deps))
		}
		for _, tg := range st.Tags {
			check := " "
			if tg.Tagged {
				check = "x"
			}
			fmt.Fprintf(&b, "- [%s] tag `%s` `%s` — %s\n", check, tg.Repo, tg.Branch, renderTagRef(tg))
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "## Status\nCurrent stage: %d/%d\nLast reconciled: %s\n\n",
		op.CurrentStage+1, len(op.Stages), now.UTC().Format(time.RFC3339))

	body, err := EmbedState(b.String(), Persistent{
		Stages:       op.Stages,
		CurrentStage: op.CurrentStage,
	})
	if err != nil {
		return "", err
	}
	return body, nil
}

func displayVersion(v string) string {
	if v == "" {
		return "_pending_"
	}
	return v
}

// renderDepList formats a Bump's bundled deps as "dep1@ver1, dep2@ver2".
// Pending deps render as "dep@_pending_".
func renderDepList(deps []DepBump) string {
	if len(deps) == 0 {
		return ""
	}
	parts := make([]string, len(deps))
	for i, d := range deps {
		parts[i] = fmt.Sprintf("%s@%s", d.Dep, displayVersion(d.Version))
	}
	return strings.Join(parts, ", ")
}

func displayState(s string) string {
	if s == "" {
		return "open"
	}
	return s
}

// renderTagRef formats a TagPrompt's trailing markdown:
//
//	tagged:    "v0.7.6"
//	pending:   "expected v0.7.6 ([run Release workflow](url))"
//	pending no expected: "_pending_ ([run Release workflow](url))"
//	pending no expected/url: "_pending_"
//
// Once Tagged the prompt collapses to just the observed version — links
// and predictions are noise after the fact.
func renderTagRef(t TagPrompt) string {
	if t.Tagged {
		if t.Version != "" {
			return t.Version
		}
		return "_tagged_"
	}
	prefix := "_pending_"
	if t.Expected != "" {
		prefix = "expected " + t.Expected
	}
	if t.WorkflowURL == "" {
		return prefix
	}
	return fmt.Sprintf("%s ([run Release workflow](%s))", prefix, t.WorkflowURL)
}

// renderBumpRef mirrors tracker.renderRef for cascade bumps.
//
// PR==0 with State=="merged" means the bump was a no-op (go.mod already at the
// target version when the bumper ran — someone bumped it manually, or a prior
// cascade run merged it). Surface that explicitly so the rendered line matches
// the ticked checkbox.
func renderBumpRef(b Bump) string {
	if b.PR == 0 {
		if b.State == "merged" {
			return "already up to date"
		}
		return "_pending_"
	}
	state := displayState(b.State)
	if b.State == "ci-failing" && b.PRURL != "" {
		state = fmt.Sprintf("[%s](%s/checks)", state, b.PRURL)
	}
	if b.PRURL == "" {
		return fmt.Sprintf("#%d (%s)", b.PR, state)
	}
	return fmt.Sprintf("[#%d](%s) (%s)", b.PR, b.PRURL, state)
}

// ExtractState pulls the metadata block out of a cascade issue body. Returns
// zero-value Persistent (no error) if absent.
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
