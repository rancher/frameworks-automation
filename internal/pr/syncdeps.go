package pr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/mod/modfile"
)

// syncDepsHook implements config.PostBundleSyncDeps. It treats the root
// go.mod as the source of truth for every module in req.SyncModules and
// propagates that version into every other go.mod under repoDir that
// already requires the module. Sub-modules that don't reference a given
// module are left alone.
//
// Source-of-truth direction: root → sub-modules. The bundle's strategies +
// the prior tidy pass have already settled what the root go.mod wants; the
// hook's job is to fan that resolution out so sibling go.mod files agree.
type syncDepsHook struct{}

func (syncDepsHook) Apply(ctx context.Context, repoDir string, req Request) error {
	if len(req.SyncModules) == 0 {
		return nil
	}
	return syncModuleVersions(ctx, repoDir, req.SyncModules)
}

func syncModuleVersions(ctx context.Context, repoDir string, modules []string) error {
	rootVersions, err := readRequireVersions(filepath.Join(repoDir, "go.mod"), modules)
	if err != nil {
		return fmt.Errorf("read root go.mod: %w", err)
	}
	dirs, err := findGoModDirs(repoDir)
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		if dir == repoDir {
			continue
		}
		for _, mod := range modules {
			ver, ok := rootVersions[mod]
			if !ok {
				continue
			}
			has, err := goModContains(filepath.Join(dir, "go.mod"), mod)
			if err != nil {
				return err
			}
			if !has {
				continue
			}
			if err := runGoGet(ctx, dir, mod, ver); err != nil {
				return err
			}
		}
	}
	return nil
}

// readRequireVersions returns the version each named module is pinned to in
// goModPath. Modules not listed in go.mod are simply absent from the map.
func readRequireVersions(goModPath string, modules []string) (map[string]string, error) {
	data, err := os.ReadFile(goModPath)
	if err != nil {
		return nil, err
	}
	f, err := modfile.Parse(goModPath, data, nil)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", goModPath, err)
	}
	wanted := make(map[string]bool, len(modules))
	for _, m := range modules {
		wanted[m] = true
	}
	out := make(map[string]string, len(modules))
	for _, r := range f.Require {
		if wanted[r.Mod.Path] {
			out[r.Mod.Path] = r.Mod.Version
		}
	}
	return out, nil
}
