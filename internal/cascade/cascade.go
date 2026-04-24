// Package cascade owns the lifecycle of cascade tracker issues. A cascade
// propagates a set of source dep versions up the dependency DAG to a target
// leaf branch, opening bump PRs one layer at a time and prompting a re-tag
// at each intermediate layer so every layer CIs against the new dep set
// before the next layer ships.
//
// Cascade is self-contained: it owns its own PRs (separate from the
// per-(dep, version) bump-op trackers) and its own Slack thread. While a
// cascade is active for a leaf branch, the reconciler's auto-dispatch path
// defers cascade-mid tags to the cascade rather than opening regular bump-op
// trackers (see pass1 coordination).
//
// Tracker identity is (leafRepo, leafBranch). Lookup is by labels:
// `cascade-op`, `leaf:<leaf-repo>:<leaf-branch>`. The source set lives in
// the metadata block — re-runs that match on explicit sources merge state;
// re-runs with a different explicit-source set supersede.
package cascade

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/rancher/release-automation/internal/config"
)

const (
	LabelOp      = "cascade-op"
	LabelLeafFmt = "leaf:%s:%s"

	stateOpen  = "<!-- cascade-op-state v1"
	stateClose = "-->"
)

// Source identifies one (dep, version) feeding the cascade.
//
// Explicit=true means the user supplied the version when triggering the
// cascade (the only valid kinds are independents). Explicit=false means
// the cascade auto-resolved the version (paired-latest: a paired dep on
// the leaf-paired branch is always picked up at its highest existing tag,
// even when the user didn't ask for an explicit bump — that's how rancher
// stays current with paired components like steve/webhook on every cascade).
//
// The Explicit subset is the supersede key: re-running the cascade with the
// same explicit sources merges state; a different explicit set supersedes.
// Paired-latest can drift between runs without forcing supersede — but once
// pinned at cascade creation, mergeState keeps it pinned across re-runs so
// the cascade doesn't move its goal post mid-flight.
type Source struct {
	Name     string `yaml:"name"`
	Version  string `yaml:"version"`
	Explicit bool   `yaml:"explicit,omitempty"`
}

// Bump is one bump-PR slot inside a cascade stage. There's exactly one Bump
// per (Repo, Branch) per stage — every in-scope dep that needs bumping at
// this layer rides in the same PR (Deps). Bundling avoids sibling-PR conflicts
// on go.mod/go.sum and matches what the cascade-mid tag CIs against: the
// combined post-bump tree.
//
// Within a Bump, each Dep's Version is fixed at cascade creation when known
// (explicit + paired-latest sources) or "" until a prior stage's tag arrives.
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
// prior stage's tag arrives. `Strategy` selects how the bump is applied to
// the working tree (default go-get); persisted so a re-run that loaded the
// state without re-reading dependencies.yaml still applies the right one.
type DepBump struct {
	Dep      string          `yaml:"dep"`
	Module   string          `yaml:"module"`
	Version  string          `yaml:"version,omitempty"`
	Strategy config.Strategy `yaml:"strategy,omitempty"`
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
	LeafRepo     string
	LeafBranch   string
	Sources      []Source
	Stages       []Stage
	CurrentStage int
}

// Persistent is what survives between reconciler runs (lives in the metadata
// block). Additive only — older runs must read newer files.
type Persistent struct {
	SlackThreadTS string   `yaml:"slack_thread_ts,omitempty"`
	Sources       []Source `yaml:"sources,omitempty"`
	Stages        []Stage  `yaml:"stages"`
	CurrentStage  int      `yaml:"current_stage"`
}

// Title is the canonical issue title for a cascade. The (leaf, branch) pair
// is the cascade's identity — the source set lives in the metadata block.
func Title(leafRepo, leafBranch string) string {
	return fmt.Sprintf("[cascade] %s %s", leafRepo, leafBranch)
}

// Labels returns the canonical label set. Source dep names are not labels:
// (a) one cascade can have many sources, (b) the source set is supersede-
// controlled (see SameExplicitSources), so labels can't track it cleanly.
func Labels(leafRepo, leafBranch string) []string {
	return []string{
		LabelOp,
		fmt.Sprintf(LabelLeafFmt, leafRepo, leafBranch),
	}
}

// LeafLabel is the leaf-axis label used by the dashboard.
func LeafLabel(leafRepo, leafBranch string) string {
	return fmt.Sprintf(LabelLeafFmt, leafRepo, leafBranch)
}

// ExplicitSources returns the user-input subset of sources, sorted by Name.
// This is the supersede comparison key.
func ExplicitSources(sources []Source) []Source {
	var out []Source
	for _, s := range sources {
		if s.Explicit {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SameExplicitSources reports whether two source slices have the same explicit
// {Name, Version} set (order-independent). Paired-latest entries are ignored:
// a re-run with the same user input shouldn't trigger supersede just because
// a paired dep cut a new tag in the meantime.
func SameExplicitSources(a, b []Source) bool {
	ax := ExplicitSources(a)
	bx := ExplicitSources(b)
	if len(ax) != len(bx) {
		return false
	}
	for i := range ax {
		if ax[i].Name != bx[i].Name || ax[i].Version != bx[i].Version {
			return false
		}
	}
	return true
}

// Render produces the cascade issue body markdown with embedded state.
// Idempotent (same Op + same time → same body).
func Render(op Op, now time.Time) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "## Cascade\n%s %s\n\n", op.LeafRepo, op.LeafBranch)

	if len(op.Sources) > 0 {
		b.WriteString("## Sources\n")
		sorted := append([]Source(nil), op.Sources...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
		for _, s := range sorted {
			kind := "paired-latest"
			if s.Explicit {
				kind = "explicit"
			}
			fmt.Fprintf(&b, "- %s %s (%s)\n", s.Name, s.Version, kind)
		}
		b.WriteString("\n")
	}

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
		Sources:      op.Sources,
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
	if b.PRURL != "" && !isTerminalState(b.State) {
		state = fmt.Sprintf("%s · [checks](%s/checks)", state, b.PRURL)
	}
	if b.PRURL == "" {
		return fmt.Sprintf("#%d (%s)", b.PR, state)
	}
	return fmt.Sprintf("[#%d](%s) (%s)", b.PR, b.PRURL, state)
}

// isTerminalState mirrors reconcile.isTerminal — duplicated here to avoid
// importing the reconcile package from cascade (would create a cycle).
func isTerminalState(s string) bool {
	return s == "merged" || s == "closed"
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
