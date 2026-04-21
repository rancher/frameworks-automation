package reconcile

import (
	"reflect"
	"testing"

	"github.com/rancher/release-automation/internal/config"
)

func TestUpstreamRepos(t *testing.T) {
	cfg := &config.Config{
		Repos: map[string]config.Repo{
			"rancher":  {Kind: config.KindLeaf, Module: "github.com/rancher/rancher", Deps: []string{"steve", "wrangler"}},
			"steve":    {Kind: config.KindPaired, Module: "github.com/rancher/steve", Deps: []string{"wrangler"}},
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
