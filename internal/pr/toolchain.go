package pr

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"golang.org/x/mod/modfile"
)

// goToolchainFor reads `<dir>/go.mod` and returns the Go toolchain name
// (e.g. "go1.25.8") that the module pins. Prefers the `toolchain`
// directive; falls back to "go"+`go` directive when no toolchain line is
// present. Returns ("", nil) when the directory has no go.mod or the
// go.mod has neither directive — caller decides what to do in that case.
//
// Why we set this explicitly: the workflow's setup-go installs the
// version pinned by THIS repo's go.mod, which in general is NOT the same
// version downstream repos want. Forcing GOTOOLCHAIN to the downstream's
// pin makes `go generate` produce the same generated artifacts the
// downstream's own CI does — without it, generator tools embed differing
// "go vX.Y" markers and the resulting bump PR carries spurious diff.
func goToolchainFor(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read go.mod: %w", err)
	}
	mf, err := modfile.Parse("go.mod", raw, nil)
	if err != nil {
		return "", fmt.Errorf("parse go.mod: %w", err)
	}
	if mf.Toolchain != nil && mf.Toolchain.Name != "" {
		return mf.Toolchain.Name, nil
	}
	if mf.Go != nil && mf.Go.Version != "" {
		return "go" + mf.Go.Version, nil
	}
	return "", nil
}

// toolchainEnv returns the env entries to inject so a subprocess `go`
// invocation in `dir` picks the toolchain that `dir`'s go.mod pins. When
// `dir`'s go.mod is absent or pins nothing (no toolchain, no go
// directive), falls back to GOTOOLCHAIN=auto so Go auto-downloads
// whatever any encountered go.mod requires — safer than inheriting the
// workflow's GOTOOLCHAIN=local default, which fails the moment a
// downstream wants a higher version than this repo's setup-go installed.
//
// Detection failures (read/parse errors) are logged and treated like the
// fallback case: missing GOTOOLCHAIN reproducibility is recoverable; a
// truly malformed go.mod will surface in the subsequent go invocation.
func toolchainEnv(dir string) []string {
	tc, err := goToolchainFor(dir)
	if err != nil {
		log.Printf("toolchainEnv: %s: %v (falling back to GOTOOLCHAIN=auto)", dir, err)
		return []string{"GOTOOLCHAIN=auto"}
	}
	if tc == "" {
		return []string{"GOTOOLCHAIN=auto"}
	}
	log.Printf("toolchainEnv: %s pins %s", dir, tc)
	return []string{"GOTOOLCHAIN=" + tc}
}
