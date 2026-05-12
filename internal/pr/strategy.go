package pr

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/rancher/release-automation/internal/config"
	"github.com/rancher/release-automation/internal/scripts"
)

// Strategy applies one (module, version) update to a working tree. Each
// implementation must be idempotent — running it twice on a tree already at
// the target version must produce no diff. The bumper relies on `git status`
// after all strategies in a bundle have run to detect the no-op case.
type Strategy interface {
	Apply(ctx context.Context, repoDir string, m Module) error
}

// strategies is the registry the bumper dispatches against. config.StrategyOrder
// is intentionally absent: order edges are sequencing-only and must be filtered
// out by callers before reaching the bumper. If one ever leaks through, the
// bumper errors with "unknown strategy" rather than silently dropping the bump.
var strategies = map[config.Strategy]Strategy{
	config.StrategyGoGet:                      goGetStrategy{},
	config.StrategyChartBump:                  scriptStrategy{name: "chart-bump", body: scripts.ChartBump},
	config.StrategyBumpWebhook:                scriptStrategy{name: "bump-webhook", body: scripts.BumpWebhook},
	config.StrategyChartBumpWebhook:           scriptStrategy{name: "chart-bump-webhook", body: scripts.ChartBumpWebhook},
	config.StrategyBumpRemotedialerProxy:      scriptStrategy{name: "bump-remotedialer-proxy", body: scripts.BumpRemotedialerProxy},
	config.StrategyChartBumpRemotedialerProxy: scriptStrategy{name: "chart-bump-remotedialer-proxy", body: scripts.ChartBumpRemotedialerProxy},
}

func lookupStrategy(s config.Strategy) (Strategy, error) {
	impl, ok := strategies[s]
	if !ok {
		return nil, fmt.Errorf("unknown bump strategy %q", s)
	}
	return impl, nil
}

// goGetStrategy runs `go get module@version` with GOFLAGS=-mod=mod so vendored
// downstreams still resolve module additions before the post-bundle tidy pass.
// It operates on every go.mod found under repoDir (vendor/ excluded) that
// already references the module, so multi-module repos are handled correctly.
type goGetStrategy struct{}

func (goGetStrategy) Apply(ctx context.Context, repoDir string, m Module) error {
	dirs, err := findGoModDirs(repoDir)
	if err != nil {
		return fmt.Errorf("find go.mod files: %w", err)
	}
	for _, dir := range dirs {
		has, err := goModContains(filepath.Join(dir, "go.mod"), m.Path)
		if err != nil {
			return err
		}
		if !has {
			continue
		}
		if err := runGoGet(ctx, dir, m.Path, m.Version); err != nil {
			return err
		}
	}
	return nil
}

// findGoModDirs walks repoDir and returns the directory of every go.mod found,
// skipping any vendor/ subtree.
func findGoModDirs(repoDir string) ([]string, error) {
	var dirs []string
	err := filepath.WalkDir(repoDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "vendor" {
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() == "go.mod" {
			dirs = append(dirs, filepath.Dir(path))
		}
		return nil
	})
	return dirs, err
}

// goModContains reports whether the go.mod at path lists modulePath as a
// dependency (require or replace). The module path in go.mod is always
// followed by a space (then a version string), so a suffix-space check avoids
// false positives from longer module names that share a common prefix.
func goModContains(goModPath, modulePath string) (bool, error) {
	data, err := os.ReadFile(goModPath)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", goModPath, err)
	}
	return strings.Contains(string(data), modulePath+" "), nil
}

// scriptStrategy materializes an embedded script body to a temp file, marks
// it executable, and runs it inside `repoDir` with the version as the sole
// argument. The script ships with the binary (see internal/scripts), so the
// downstream repo doesn't need it pre-installed.
//
// `name` is used in the temp filename and error messages for clarity in CI
// logs (e.g. "release-automation-chart-bump-*.sh").
type scriptStrategy struct {
	name string
	body string
}

func (s scriptStrategy) Apply(ctx context.Context, repoDir string, m Module) error {
	if s.body == "" {
		return fmt.Errorf("script strategy %q: empty body", s.name)
	}
	f, err := os.CreateTemp("", "release-automation-"+s.name+"-*.sh")
	if err != nil {
		return fmt.Errorf("script strategy %q: create temp: %w", s.name, err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(s.body); err != nil {
		f.Close()
		return fmt.Errorf("script strategy %q: write: %w", s.name, err)
	}
	if err := f.Chmod(0o700); err != nil {
		f.Close()
		return fmt.Errorf("script strategy %q: chmod: %w", s.name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("script strategy %q: close: %w", s.name, err)
	}
	return run(ctx, repoDir, toolchainEnv(repoDir), f.Name(), m.Version)
}
