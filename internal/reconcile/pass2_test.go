package reconcile

import (
	"testing"

	ghclient "github.com/rancher/release-automation/internal/github"
	"github.com/rancher/release-automation/internal/tracker"
)

func TestDepFromLabels(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   string
	}{
		{"present", []string{"bump-op", "dep:wrangler"}, "wrangler"},
		{"missing", []string{"bump-op"}, ""},
		{"first wins", []string{"dep:steve", "dep:rancher"}, "steve"},
		{"empty", nil, ""},
	}
	for _, c := range cases {
		if got := depFromLabels(c.labels); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestDerivePRState(t *testing.T) {
	cases := []struct {
		name string
		pr   *ghclient.PR
		want string
	}{
		{"merged wins over closed", &ghclient.PR{State: "closed", Merged: true}, "merged"},
		{"closed not merged", &ghclient.PR{State: "closed"}, "closed"},
		{"open", &ghclient.PR{State: "open"}, "open"},
	}
	for _, c := range cases {
		if got := derivePRState(c.pr); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestAllTerminal(t *testing.T) {
	cases := []struct {
		name    string
		targets []tracker.Target
		want    bool
	}{
		{"empty", nil, false},
		{"all merged", []tracker.Target{{PR: 1, State: "merged"}, {PR: 2, State: "merged"}}, true},
		{"merged + closed", []tracker.Target{{PR: 1, State: "merged"}, {PR: 2, State: "closed"}}, true},
		{"one still open", []tracker.Target{{PR: 1, State: "merged"}, {PR: 2, State: "open"}}, false},
		{"pending PR", []tracker.Target{{PR: 1, State: "merged"}, {PR: 0}}, false},
	}
	for _, c := range cases {
		if got := allTerminal(c.targets); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
