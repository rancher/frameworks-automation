package main

import (
	"strings"
	"testing"

	"github.com/rancher/release-automation/internal/config"
)

func leafConfig(md string) *config.Config {
	return &config.Config{Repos: map[string]config.Repo{
		"rancher": {Kind: config.KindLeaf, Repo: "rancher/rancher", VersionMD: md},
	}}
}

func TestPickCascadeConfig(t *testing.T) {
	cfgs := map[string]*config.Config{
		"legacy": leafConfig("| Branch | Minor |\n|---|---|\n| release/v2.15 | v2.15 |\n| release/v2.14 | v2.14 |\n"),
		"v2":     leafConfig("| Branch | Minor |\n|---|---|\n| main | v2.17 |\n| release/v2.16 | v2.16 |\n"),
		"dup":    leafConfig("| Branch | Minor |\n|---|---|\n| main | v2.17 |\n"),
		"fetched": {Repos: map[string]config.Repo{
			"rancher": {Kind: config.KindLeaf, Repo: "rancher/rancher"},
		}},
	}
	tests := []struct {
		names      string
		leafBranch string
		want       string
		wantErr    string
	}{
		{names: "legacy,v2", leafBranch: "main", want: "v2"},
		{names: "legacy,v2", leafBranch: "release/v2.16", want: "v2"},
		{names: "legacy,v2", leafBranch: "release/v2.15", want: "legacy"},
		{names: "legacy, v2", leafBranch: "release/v2.14", want: "legacy"},
		{names: "legacy,v2", leafBranch: "release/v2.9", wantErr: "in none of the configs"},
		{names: "v2,dup", leafBranch: "main", wantErr: "in more than one config"},
		// A single name is passed through even when its table doesn't list
		// the branch, so RunCascade reports it as before.
		{names: "legacy", leafBranch: "main", want: "legacy"},
		{names: "fetched", leafBranch: "main", want: "fetched"},
		{names: "legacy,fetched", leafBranch: "main", wantErr: "no inline version-md"},
		{names: "legacy,nope", leafBranch: "main", wantErr: `unknown config "nope"`},
	}
	for _, tt := range tests {
		t.Run(tt.names+"@"+tt.leafBranch, func(t *testing.T) {
			got, err := pickCascadeConfig(cfgs, strings.Split(tt.names, ","), tt.leafBranch)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

// TestPickCascadeConfig_WebhookConfigs checks the real webhook configs: every
// rancher branch is in exactly one of the legacy (rancher/charts dev-v2.X)
// and charts v2 configs, for both the rc and the unrc workflow.
func TestPickCascadeConfig_WebhookConfigs(t *testing.T) {
	cfgs, err := config.LoadAll("../../dependencies")
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	for _, pair := range [][2]string{
		{"rancher-webhook-legacy", "rancher-webhook"},
		{"rancher-webhook-legacy-unrc", "rancher-webhook-unrc"},
	} {
		names := pair[:]
		for branch, want := range map[string]string{
			"main":          pair[1],
			"release/v2.16": pair[1],
			"release/v2.15": pair[0],
			"release/v2.10": pair[0],
		} {
			got, err := pickCascadeConfig(cfgs, append([]string(nil), names...), branch)
			if err != nil || got != want {
				t.Errorf("%v @ %s: got %q, %v; want %q", names, branch, got, err, want)
			}
		}
	}
}
