package cascade

import (
	"context"
	"fmt"
	"time"

	ghclient "github.com/rancher/release-automation/internal/github"
)

// Issue is a thin wrapper carrying both the rendered body and the GitHub
// issue identity.
type Issue struct {
	Number int
	Title  string
	URL    string
	Body   string
}

// FindOrCreate looks up the open cascade for (op.Dep, op.Version, op.LeafRepo,
// op.LeafBranch). If absent, creates one rendered from `op`. If present,
// merges any state already stored in its metadata block back into `op` so
// the caller sees existing PR links.
//
// Lookup: query labels (cascade-op + dep + leaf) and filter by version
// parsed from the title. Equal-version trackers dedupe; older versions
// for the same (dep, leaf) get superseded by Supersede separately.
func FindOrCreate(ctx context.Context, gh *ghclient.Client, automationRepo string, op *Op) (*Issue, error) {
	labels := Labels(op.Dep, op.LeafRepo, op.LeafBranch)
	candidates, err := gh.ListOpenIssues(ctx, automationRepo, labels)
	if err != nil {
		return nil, fmt.Errorf("find cascade for %s %s on %s %s: %w", op.Dep, op.Version, op.LeafRepo, op.LeafBranch, err)
	}
	for _, existing := range candidates {
		if ParseVersionFromTitle(existing.Title, op.Dep) != op.Version {
			continue
		}
		st, err := ExtractState(existing.Body)
		if err != nil {
			return nil, fmt.Errorf("read state from cascade #%d: %w", existing.Number, err)
		}
		mergeState(op, st)
		return &Issue{Number: existing.Number, Title: existing.Title, URL: existing.URL, Body: existing.Body}, nil
	}

	body, err := renderForCreate(*op)
	if err != nil {
		return nil, err
	}
	created, err := gh.CreateIssue(ctx, automationRepo, Title(op.Dep, op.Version, op.LeafRepo, op.LeafBranch), body, labels)
	if err != nil {
		return nil, fmt.Errorf("create cascade for %s %s on %s %s: %w", op.Dep, op.Version, op.LeafRepo, op.LeafBranch, err)
	}
	return &Issue{Number: created.Number, Title: created.Title, URL: created.URL, Body: body}, nil
}

// UpdateBody re-renders `op` and pushes the new body to the cascade issue.
func UpdateBody(ctx context.Context, gh *ghclient.Client, automationRepo string, issueNum int, op Op) error {
	body, err := renderForCreate(op)
	if err != nil {
		return err
	}
	return gh.UpdateIssueBody(ctx, automationRepo, issueNum, body)
}

// mergeState reconciles op.Stages with what's already stored in the tracker.
// Stage shape (layer, repos, deps) is determined at cascade creation by
// ComputeStages and shouldn't drift across runs — so we overlay PR/version/
// state from `st` onto matching stage/bump positions and adopt CurrentStage
// from `st`. Mismatched shapes are kept from `op` (the recompute) on the
// theory that a config change since cascade creation should be visible.
func mergeState(op *Op, st Persistent) {
	op.CurrentStage = st.CurrentStage
	for i := range op.Stages {
		if i >= len(st.Stages) {
			break
		}
		stored := st.Stages[i]
		// Bumps overlay by (Repo, Branch, Dep) — the stable identity of a
		// bump within a stage.
		idx := make(map[string]int, len(stored.Bumps))
		for j, bp := range stored.Bumps {
			idx[bp.Repo+"|"+bp.Branch+"|"+bp.Dep] = j
		}
		for k := range op.Stages[i].Bumps {
			cur := op.Stages[i].Bumps[k]
			if j, ok := idx[cur.Repo+"|"+cur.Branch+"|"+cur.Dep]; ok {
				stb := stored.Bumps[j]
				if stb.Version != "" {
					op.Stages[i].Bumps[k].Version = stb.Version
				}
				op.Stages[i].Bumps[k].PR = stb.PR
				op.Stages[i].Bumps[k].PRURL = stb.PRURL
				op.Stages[i].Bumps[k].State = stb.State
			}
		}
		// Tags overlay by (Repo, Branch) — what we wait on.
		tidx := make(map[string]int, len(stored.Tags))
		for j, tg := range stored.Tags {
			tidx[tg.Repo+"|"+tg.Branch] = j
		}
		for k := range op.Stages[i].Tags {
			cur := op.Stages[i].Tags[k]
			if j, ok := tidx[cur.Repo+"|"+cur.Branch]; ok {
				op.Stages[i].Tags[k].Version = stored.Tags[j].Version
				op.Stages[i].Tags[k].Tagged = stored.Tags[j].Tagged
			}
		}
	}
}

// renderForCreate is Render but uses the package-level clock so tests can
// pin the timestamp.
var renderForCreate = func(op Op) (string, error) {
	return Render(op, nowFn())
}

var nowFn = func() time.Time { return time.Now() }
