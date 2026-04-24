package reconcile

import (
	"reflect"
	"testing"

	"github.com/rancher/release-automation/internal/config"
)

func TestUpstreamRepos(t *testing.T) {
	cfg := &config.Config{
		Repos: map[string]config.Repo{
			"rancher":  {Kind: config.KindLeaf, Module: "github.com/rancher/rancher", Deps: []config.Dep{{Name: "steve", Strategy: config.StrategyGoGet}, {Name: "wrangler", Strategy: config.StrategyGoGet}}},
			"steve":    {Kind: config.KindPaired, Module: "github.com/rancher/steve", Deps: []config.Dep{{Name: "wrangler", Strategy: config.StrategyGoGet}}},
			"wrangler": {Kind: config.KindIndependent, Module: "github.com/rancher/wrangler"},
			"orphan":   {Kind: config.KindIndependent, Module: "github.com/rancher/orphan"}, // no one depends on it
		},
	}
	got := upstreamRepos(cfg)
	want := []string{"steve", "wrangler"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
