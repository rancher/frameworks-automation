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

// Bump is one bump-PR slot inside a cascade stage. A stage may have several
// (e.g. final stage in a fan-in DAG bumps every direct in-scope dep into the
// leaf). Versions for stage-1 bumps are fixed at cascade creation; later
// stages have Version == "" until prior stages emit tags.
type Bump struct {
	Repo    string `yaml:"repo"`
	Branch  string `yaml:"branch"`
	Dep     string `yaml:"dep"`              // which dep is being bumped here
	Module  string `yaml:"module"`           // Go module path of Dep
	Version string `yaml:"version,omitempty"`
	PR      int    `yaml:"pr,omitempty"`
	PRURL   string `yaml:"pr_url,omitempty"`
	State   string `yaml:"state,omitempty"`  // "" | open | ci-failing | approved | merged | closed
}

// TagPrompt is one tag the cascade waits on at the end of a non-final stage.
// Version is observed (not predicted) — the dev picks the next-patch when
// running the per-repo Release workflow.
type TagPrompt struct {
	Repo    string `yaml:"repo"`
	Branch  string `yaml:"branch"`
	Version string `yaml:"version,omitempty"`
	Tagged  bool   `yaml:"tagged,omitempty"`
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
			fmt.Fprintf(&b, "- [%s] bump `%s` `%s` — %s (%s@%s)\n", check, bp.Repo, bp.Branch, renderBumpRef(bp), bp.Dep, displayVersion(bp.Version))
		}
		for _, tg := range st.Tags {
			check := " "
			if tg.Tagged {
				check = "x"
			}
			ver := displayVersion(tg.Version)
			fmt.Fprintf(&b, "- [%s] tag `%s` `%s` — %s\n", check, tg.Repo, tg.Branch, ver)
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

func displayState(s string) string {
	if s == "" {
		return "open"
	}
	return s
}

// renderBumpRef mirrors tracker.renderRef for cascade bumps.
func renderBumpRef(b Bump) string {
	if b.PR == 0 {
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
