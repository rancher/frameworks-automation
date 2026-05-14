package pr

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadRequireVersions(t *testing.T) {
	dir := t.TempDir()
	gomod := filepath.Join(dir, "go.mod")
	content := `module root

go 1.21

require (
	github.com/rancher/steve v0.7.4
	github.com/rancher/wrangler/v3 v3.2.1
	github.com/unrelated/lib v1.0.0
)
`
	if err := os.WriteFile(gomod, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := readRequireVersions(gomod, []string{
		"github.com/rancher/steve",
		"github.com/rancher/wrangler/v3",
		"github.com/rancher/missing",
	})
	if err != nil {
		t.Fatalf("readRequireVersions: %v", err)
	}
	if got["github.com/rancher/steve"] != "v0.7.4" {
		t.Errorf("steve: got %q want v0.7.4", got["github.com/rancher/steve"])
	}
	if got["github.com/rancher/wrangler/v3"] != "v3.2.1" {
		t.Errorf("wrangler: got %q want v3.2.1", got["github.com/rancher/wrangler/v3"])
	}
	if _, ok := got["github.com/rancher/missing"]; ok {
		t.Error("missing module must not appear in result")
	}
	if _, ok := got["github.com/unrelated/lib"]; ok {
		t.Error("non-requested module must not appear in result")
	}
}
