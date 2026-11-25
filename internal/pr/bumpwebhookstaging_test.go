package pr

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rancher/release-automation/internal/scripts"
)

// stagingIndex mimics rancher/charts' charts v2 staging index.yaml: one index
// for every Rancher release line, so the same webhook appVersion can appear
// once per line, told apart by catalog.cattle.io/rancher-version. The two
// 0.14.0 entries on 2.18 are a broken index the script must refuse.
const stagingIndex = `apiVersion: v1
entries:
  rancher-webhook:
  - version: 112.0.0+up0.13.0-rc.11
    appVersion: 0.13.0-rc.11
    annotations:
      catalog.cattle.io/rancher-version: '>= 2.17.0-0 < 2.18.0-0'
  - version: 111.0.9+up0.13.0-rc.11
    appVersion: 0.13.0-rc.11
    annotations:
      catalog.cattle.io/rancher-version: '>= 2.16.0-0 < 2.17.0-0'
  - version: 111.0.1+up0.12.1
    appVersion: 0.12.1
    annotations:
      catalog.cattle.io/rancher-version: '>= 2.16.0-0 < 2.17.0-0'
  - version: 110.0.5+up0.11.5
    appVersion: 0.11.5
    annotations:
      catalog.cattle.io/rancher-version: '>= 2.15.0-0 < 2.16.0-0'
  - version: 113.0.0+up0.14.0
    appVersion: 0.14.0
    annotations:
      catalog.cattle.io/rancher-version: '>= 2.18.0-0 < 2.19.0-0'
  - version: 113.0.1+up0.14.0
    appVersion: 0.14.0
    annotations:
      catalog.cattle.io/rancher-version: '>= 2.18.0-0 < 2.19.0-0'
`

// TestBumpWebhookStagingScript runs the embedded bump-webhook-staging script
// through scriptStrategy against a fake rancher tree, with `curl` serving
// stagingIndex and `go` stubbed: only the entries of the RANCHER_MINOR line
// count, and exactly one must match.
func TestBumpWebhookStagingScript(t *testing.T) {
	if _, err := exec.LookPath("yq"); err != nil {
		t.Skip("yq not on PATH")
	}
	stubTools(t, "go")
	bin := t.TempDir()
	index := filepath.Join(bin, "index.yaml")
	writeFile(t, index, stagingIndex)
	writeFile(t, filepath.Join(bin, "curl"), "#!/bin/sh\ncat '"+index+"'\n")
	if err := os.Chmod(filepath.Join(bin, "curl"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	tests := []struct {
		name         string
		version      string
		rancherMinor string
		want         string
		wantErr      string
	}{
		{name: "2.17 line", version: "v0.13.0-rc.11", rancherMinor: "2.17", want: "112.0.0+up0.13.0-rc.11"},
		{name: "same appVersion on the 2.16 line", version: "v0.13.0-rc.11", rancherMinor: "2.16", want: "111.0.9+up0.13.0-rc.11"},
		{name: "2.16 line", version: "v0.12.1", rancherMinor: "2.16", want: "111.0.1+up0.12.1"},
		{name: "not staged for that line", version: "v0.12.1", rancherMinor: "2.17", wantErr: "no rancher-webhook entry with appVersion=0.12.1 for Rancher 2.17"},
		{name: "minor is not a prefix match", version: "v0.13.0-rc.11", rancherMinor: "2.1", wantErr: "no rancher-webhook entry"},
		{name: "two entries for the line", version: "v0.14.0", rancherMinor: "2.18", wantErr: "more than one rancher-webhook entry"},
		{name: "missing RANCHER_MINOR", version: "v0.12.1", wantErr: "RANCHER_MINOR env var must be"},
		{name: "malformed RANCHER_MINOR", version: "v0.12.1", rancherMinor: "v2.16", wantErr: "RANCHER_MINOR env var must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			const prev = "webhookVersion: 100.0.0+up0.1.0\n"
			writeFile(t, filepath.Join(dir, "build.yaml"), prev)

			s := scriptStrategy{name: "bump-webhook-staging", body: scripts.BumpWebhookStaging}
			var err error
			stderr := captureStderr(t, func() {
				err = s.Apply(context.Background(), dir, Module{Version: tt.version, RancherMinor: tt.rancherMinor})
			})
			got := readFile(t, filepath.Join(dir, "build.yaml"))
			if tt.wantErr != "" {
				// run() streams the script's stderr to the CI log rather than
				// into the error, so the message is read from there.
				if err == nil || !strings.Contains(stderr, tt.wantErr) {
					t.Fatalf("Apply error = %v, stderr %q, want containing %q", err, stderr, tt.wantErr)
				}
				if got != prev {
					t.Errorf("build.yaml modified on error: %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if want := "webhookVersion: " + tt.want + "\n"; got != want {
				t.Errorf("build.yaml = %q, want %q", got, want)
			}
		})
	}
}

// captureStderr returns what fn writes to os.Stderr, which run() hands to
// the scripts it starts.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stderr = orig
	w.Close()
	return <-done
}
