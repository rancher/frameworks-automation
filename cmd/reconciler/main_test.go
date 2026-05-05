package main

import (
	"strings"
	"testing"

	"github.com/rancher/release-automation/internal/config"
)

func TestParseTagStrategyOverride(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]config.NextTagStrategy
	}{
		{"empty", "", map[string]config.NextTagStrategy{}},
		{"single", "webhook=unrc", map[string]config.NextTagStrategy{"webhook": config.NextTagUnRC}},
		{
			"multiple",
			"webhook=unrc,remotedialer-proxy=rc",
			map[string]config.NextTagStrategy{
				"webhook":            config.NextTagUnRC,
				"remotedialer-proxy": config.NextTagRC,
			},
		},
		{"trailing comma tolerated", "webhook=unrc,", map[string]config.NextTagStrategy{"webhook": config.NextTagUnRC}},
		{"whitespace tolerated", "  webhook = unrc , ", map[string]config.NextTagStrategy{"webhook": config.NextTagUnRC}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTagStrategyOverride(tt.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("got[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestParseTagStrategyOverride_Errors(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantSub string
	}{
		{"missing equals", "webhook", "want repo=strategy"},
		{"empty repo", "=unrc", "want repo=strategy"},
		{"empty strategy", "webhook=", "repo and strategy are both required"},
		{"unknown strategy", "webhook=bogus", "unknown strategy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseTagStrategyOverride(tt.in)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tt.wantSub)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("want error containing %q, got %v", tt.wantSub, err)
			}
		})
	}
}
