package cascade

import (
	"testing"
)

func TestMergeState_OverlaysPRAndTagAndCurrentStage(t *testing.T) {
	op := &Op{
		Stages: []Stage{
			{Layer: 1,
				Bumps: []Bump{{Repo: "steve", Branch: "main", Dep: "wrangler", Module: "github.com/x/wrangler", Version: "v0.5.1"}},
				Tags:  []TagPrompt{{Repo: "steve", Branch: "main"}},
			},
			{Layer: 2,
				Bumps: []Bump{{Repo: "rancher", Branch: "main", Dep: "steve", Module: "github.com/x/steve"}},
			},
		},
	}
	stored := Persistent{
		CurrentStage: 1,
		Stages: []Stage{
			{Layer: 1,
				Bumps: []Bump{{Repo: "steve", Branch: "main", Dep: "wrangler", Module: "github.com/x/wrangler", Version: "v0.5.1", PR: 42, PRURL: "https://x/42", State: "merged"}},
				Tags:  []TagPrompt{{Repo: "steve", Branch: "main", Version: "v0.7.6", Tagged: true}},
			},
			{Layer: 2,
				Bumps: []Bump{{Repo: "rancher", Branch: "main", Dep: "steve", Module: "github.com/x/steve", Version: "v0.7.6", PR: 99, State: "open"}},
			},
		},
	}
	mergeState(op, stored)
	if op.CurrentStage != 1 {
		t.Errorf("CurrentStage: got %d want 1", op.CurrentStage)
	}
	if op.Stages[0].Bumps[0].PR != 42 || op.Stages[0].Bumps[0].State != "merged" {
		t.Errorf("stage0 bump not merged in: %+v", op.Stages[0].Bumps[0])
	}
	if !op.Stages[0].Tags[0].Tagged || op.Stages[0].Tags[0].Version != "v0.7.6" {
		t.Errorf("stage0 tag not merged in: %+v", op.Stages[0].Tags[0])
	}
	if op.Stages[1].Bumps[0].Version != "v0.7.6" || op.Stages[1].Bumps[0].PR != 99 {
		t.Errorf("stage1 bump not merged in: %+v", op.Stages[1].Bumps[0])
	}
}

func TestMergeState_UnknownBumpsKeepOpVersion(t *testing.T) {
	// op has a fresh stage1 bump (no version yet); stored has a different
	// bump (different Dep) — overlays only on matching identity.
	op := &Op{
		Stages: []Stage{{Layer: 2, Bumps: []Bump{{Repo: "rancher", Branch: "main", Dep: "steve", Module: "github.com/x/steve"}}}},
	}
	stored := Persistent{
		Stages: []Stage{{Layer: 2, Bumps: []Bump{{Repo: "rancher", Branch: "main", Dep: "wrangler", Module: "github.com/x/wrangler", Version: "v0.5.1", PR: 7}}}},
	}
	mergeState(op, stored)
	if op.Stages[0].Bumps[0].PR != 0 || op.Stages[0].Bumps[0].Version != "" {
		t.Errorf("unrelated stored bump should not overlay: %+v", op.Stages[0].Bumps[0])
	}
}
