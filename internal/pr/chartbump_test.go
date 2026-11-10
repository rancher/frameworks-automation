package pr

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rancher/release-automation/internal/scripts"
)

// TestChartBumpScripts_ChartMajor runs the embedded chart-bump-webhook and
// chart-bump-remotedialer-proxy scripts through scriptStrategy against a
// fake rancher/charts tree, checking how the chart version is picked from
// CHART_MAJOR (the "Chart Major Version" column of the dep's VERSION.md).
//
// The "branch cut" cases are the scenario this exists for: dev-v2.17 is cut
// from dev-v2.16, so package.yaml still carries 111.x.y when the first
// v0.13 / v0.10 bump arrives and must reset to 112.0.0.
//
// `git` and `make` are stubbed on PATH (the scripts fetch automation-core
// and run `make charts`); yq must be real.
func TestChartBumpScripts_ChartMajor(t *testing.T) {
	if _, err := exec.LookPath("yq"); err != nil {
		t.Skip("yq not on PATH")
	}
	stubTools(t, "git", "make")

	charts := []struct {
		name    string
		body    string
		pkg     string // packages/<pkg> and release.yaml key
		project string // GitHub release URL project
	}{
		{name: "chart-bump-webhook", body: scripts.ChartBumpWebhook, pkg: "rancher-webhook", project: "webhook"},
		{name: "chart-bump-remotedialer-proxy", body: scripts.ChartBumpRemotedialerProxy, pkg: "remotedialer-proxy", project: "remotedialer-proxy"},
	}
	tests := []struct {
		name         string
		prevChart    string
		prevVersion  string
		newVersion   string
		chartMajor   string
		wantChart    string // "" with wantErr
		wantErr      string
		wantNoChange bool
	}{
		{name: "branch cut from rc resets to new major", prevChart: "111.0.0", prevVersion: "v0.12.1-rc.17", newVersion: "v0.13.0-rc.1", chartMajor: "112", wantChart: "112.0.0"},
		{name: "branch cut from GA resets minor and patch", prevChart: "111.2.3", prevVersion: "v0.12.4", newVersion: "v0.13.0", chartMajor: "112", wantChart: "112.0.0"},
		{name: "branch cut skipping majors", prevChart: "106.0.5", prevVersion: "v0.5.4", newVersion: "v0.6.0-rc.1", chartMajor: "108", wantChart: "108.0.0"},
		{name: "same major after rc keeps version", prevChart: "112.0.0", prevVersion: "v0.13.0-rc.1", newVersion: "v0.13.0-rc.2", chartMajor: "112", wantChart: "112.0.0"},
		{name: "same major after GA bumps patch", prevChart: "112.0.0", prevVersion: "v0.13.0", newVersion: "v0.13.1-rc.1", chartMajor: "112", wantChart: "112.0.1"},
		{name: "no CHART_MAJOR keeps legacy rc behaviour", prevChart: "111.0.0", prevVersion: "v0.12.1-rc.17", newVersion: "v0.13.0-rc.1", wantChart: "111.0.0"},
		{name: "no CHART_MAJOR keeps legacy GA behaviour", prevChart: "111.0.4", prevVersion: "v0.12.4", newVersion: "v0.12.5-rc.1", wantChart: "111.0.5"},
		{name: "same version is a no-op", prevChart: "112.0.0", prevVersion: "v0.13.0-rc.1", newVersion: "v0.13.0-rc.1", chartMajor: "112", wantNoChange: true},
		{name: "package major newer than CHART_MAJOR errors", prevChart: "112.0.0", prevVersion: "v0.13.0-rc.1", newVersion: "v0.13.0-rc.2", chartMajor: "111", wantErr: "exit status 1"},
		{name: "non-numeric CHART_MAJOR errors", prevChart: "111.0.0", prevVersion: "v0.12.1-rc.17", newVersion: "v0.13.0-rc.1", chartMajor: "11x", wantErr: "exit status 1"},
	}
	for _, c := range charts {
		for _, tt := range tests {
			t.Run(c.name+"/"+tt.name, func(t *testing.T) {
				dir := t.TempDir()
				prevShort := strings.TrimPrefix(tt.prevVersion, "v")
				prevPkg := "url: https://github.com/rancher/" + c.project + "/releases/download/" + tt.prevVersion + "/" + c.pkg + "-" + prevShort + ".tgz\n" +
					"version: " + tt.prevChart + "\n"
				prevRelease := c.pkg + ":\n  - " + tt.prevChart + "+up" + prevShort + "\n"
				pkgPath := filepath.Join(dir, "packages", c.pkg, "package.yaml")
				writeFile(t, pkgPath, prevPkg)
				writeFile(t, filepath.Join(dir, "release.yaml"), prevRelease)

				s := scriptStrategy{name: c.name, body: c.body}
				err := s.Apply(context.Background(), dir, Module{Version: tt.newVersion, ChartMajor: tt.chartMajor})
				if tt.wantErr != "" {
					if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
						t.Fatalf("Apply error = %v, want containing %q", err, tt.wantErr)
					}
					if got := readFile(t, pkgPath); got != prevPkg {
						t.Errorf("package.yaml modified on error:\n%s", got)
					}
					return
				}
				if err != nil {
					t.Fatalf("Apply: %v", err)
				}

				gotPkg := readFile(t, pkgPath)
				gotRelease := readFile(t, filepath.Join(dir, "release.yaml"))
				if tt.wantNoChange {
					if gotPkg != prevPkg || gotRelease != prevRelease {
						t.Errorf("expected no change, got package.yaml:\n%s\nrelease.yaml:\n%s", gotPkg, gotRelease)
					}
					return
				}

				newShort := strings.TrimPrefix(tt.newVersion, "v")
				wantPkg := "url: https://github.com/rancher/" + c.project + "/releases/download/" + tt.newVersion + "/" + c.pkg + "-" + newShort + ".tgz\n" +
					"version: " + tt.wantChart + "\n"
				if gotPkg != wantPkg {
					t.Errorf("package.yaml:\n got: %q\nwant: %q", gotPkg, wantPkg)
				}
				wantEntry := tt.wantChart + "+up" + newShort
				if first := firstReleaseEntry(gotRelease); first != wantEntry {
					t.Errorf("release.yaml first entry = %q, want %q\n%s", first, wantEntry, gotRelease)
				}

				// Re-running the same bump must be a no-op (bumper contract).
				if err := s.Apply(context.Background(), dir, Module{Version: tt.newVersion, ChartMajor: tt.chartMajor}); err != nil {
					t.Fatalf("re-run Apply: %v", err)
				}
				if again := readFile(t, pkgPath); again != gotPkg {
					t.Errorf("re-run changed package.yaml:\n got: %q\nwant: %q", again, gotPkg)
				}
				if again := readFile(t, filepath.Join(dir, "release.yaml")); again != gotRelease {
					t.Errorf("re-run changed release.yaml:\n got: %q\nwant: %q", again, gotRelease)
				}
			})
		}
	}
}

// stubTools puts no-op executables named `names` first on PATH.
func stubTools(t *testing.T, names ...string) {
	t.Helper()
	bin := t.TempDir()
	for _, n := range names {
		writeFile(t, filepath.Join(bin, n), "#!/bin/sh\nexit 0\n")
		if err := os.Chmod(filepath.Join(bin, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// firstReleaseEntry returns the first "  - <entry>" item in release.yaml.
func firstReleaseEntry(release string) string {
	for _, l := range strings.Split(release, "\n") {
		if e, ok := strings.CutPrefix(strings.TrimSpace(l), "- "); ok {
			return e
		}
	}
	return ""
}
