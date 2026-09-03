package renovateapprove

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "renovateapprove.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	return path
}

func TestLoad_Defaults(t *testing.T) {
	path := writeConfig(t, `
repos:
  - repo: rancher/steve
    token-env: GH_TOKEN_STEVE
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Author != "renovate-rancher[bot]" {
		t.Errorf("Author default = %q", cfg.Author)
	}
	if cfg.BranchPrefix != "renovate/" {
		t.Errorf("BranchPrefix default = %q", cfg.BranchPrefix)
	}
	if cfg.MinWorkingDays != 1 {
		t.Errorf("MinWorkingDays default = %d", cfg.MinWorkingDays)
	}
}

func TestLoad_RejectsMissingRepos(t *testing.T) {
	path := writeConfig(t, "repos: []\n")
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for empty repos list")
	}
}

func TestLoad_RejectsMalformedRepo(t *testing.T) {
	path := writeConfig(t, `
repos:
  - repo: not-owner-slash-name
    token-env: GH_TOKEN_X
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for malformed repo")
	}
}

func TestLoad_RejectsMissingTokenEnv(t *testing.T) {
	path := writeConfig(t, `
repos:
  - repo: rancher/steve
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for missing token-env")
	}
}

func TestLoad_RejectsDuplicateRepo(t *testing.T) {
	path := writeConfig(t, `
repos:
  - repo: rancher/steve
    token-env: GH_TOKEN_STEVE
  - repo: rancher/steve
    token-env: GH_TOKEN_STEVE_2
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for duplicate repo")
	}
}

func TestConfig_Tokens(t *testing.T) {
	cfg := &Config{Repos: []RepoConfig{
		{Repo: "rancher/steve", TokenEnv: "GH_TOKEN_STEVE"},
		{Repo: "rancher/webhook", TokenEnv: "GH_TOKEN_WEBHOOK"},
	}}
	env := map[string]string{"GH_TOKEN_STEVE": "s3cr3t", "GH_TOKEN_WEBHOOK": "s3cr3t2"}
	tokens, err := cfg.Tokens(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("Tokens: %v", err)
	}
	if tokens["rancher/steve"] != "s3cr3t" || tokens["rancher/webhook"] != "s3cr3t2" {
		t.Errorf("unexpected tokens: %+v", tokens)
	}
}

func TestConfig_Tokens_MissingEnv(t *testing.T) {
	cfg := &Config{Repos: []RepoConfig{{Repo: "rancher/steve", TokenEnv: "GH_TOKEN_STEVE"}}}
	if _, err := cfg.Tokens(func(string) string { return "" }); err == nil {
		t.Fatal("expected error for missing env var")
	}
}
