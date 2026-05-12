package pr

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGoToolchainFor(t *testing.T) {
	cases := []struct {
		name    string
		gomod   string // "" means: don't write a go.mod
		want    string
		wantErr bool
	}{
		{
			name:  "toolchain directive wins over go directive",
			gomod: "module x\n\ngo 1.25.0\n\ntoolchain go1.25.8\n",
			want:  "go1.25.8",
		},
		{
			name:  "go directive only",
			gomod: "module x\n\ngo 1.25.0\n",
			want:  "go1.25.0",
		},
		{
			name: "no go.mod present",
			want: "",
		},
		{
			name:    "malformed go.mod errors",
			gomod:   "this is not a valid go.mod\n",
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if c.gomod != "" {
				if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(c.gomod), 0o644); err != nil {
					t.Fatalf("write go.mod: %v", err)
				}
			}
			got, err := goToolchainFor(dir)
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, c.wantErr)
			}
			if got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

// TestToolchainEnv_FallbackToAuto pins the contract that callers can rely
// on: when the directory has no go.mod (or no useful directive), we still
// inject GOTOOLCHAIN=auto rather than letting the subprocess inherit the
// runner's GOTOOLCHAIN=local — local would refuse to satisfy a downstream
// pin higher than the version setup-go installed for this repo.
func TestToolchainEnv_FallbackToAuto(t *testing.T) {
	dir := t.TempDir() // no go.mod
	env := toolchainEnv(dir)
	if len(env) != 1 || env[0] != "GOTOOLCHAIN=auto" {
		t.Errorf("missing-go.mod fallback: got %v want [GOTOOLCHAIN=auto]", env)
	}
}

func TestToolchainEnv_FromToolchainDirective(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module x\n\ngo 1.25.0\n\ntoolchain go1.25.8\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	env := toolchainEnv(dir)
	if len(env) != 1 || env[0] != "GOTOOLCHAIN=go1.25.8" {
		t.Errorf("got %v want [GOTOOLCHAIN=go1.25.8]", env)
	}
}
