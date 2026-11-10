package config

import "testing"

const steveVersionMD = `Steve follows a pre-release (v0.x) strategy of semver. There is no compatibility between releases, and new releases are meant to be strictly compatible with a single version of Rancher. The current supported versions of Steve are as follows:

| Steve Branch | Steve Minor version | Matching Rancher Version |
|--------------|---------------------|--------------------------|
| main | v0.9 | v2.15 |
| release/v0.8 | v0.8 | v2.14 |
| release/v0.7 | v0.7 | v2.13 |
| release/v0.6 | v0.6 | v2.12 |
`

func TestParseVersionTable_Steve(t *testing.T) {
	tbl, err := ParseVersionTable(steveVersionMD)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, want := len(tbl.Rows), 4; got != want {
		t.Fatalf("rows: got %d want %d", got, want)
	}
	cases := []struct {
		branch, minor, pair string
	}{
		{"main", "v0.9", "v2.15"},
		{"release/v0.8", "v0.8", "v2.14"},
		{"release/v0.7", "v0.7", "v2.13"},
		{"release/v0.6", "v0.6", "v2.12"},
	}
	for i, c := range cases {
		r := tbl.Rows[i]
		if r.Branch != c.branch || r.Minor != c.minor || r.Pair != c.pair {
			t.Errorf("row %d: got %+v want %+v", i, r, c)
		}
	}
	if got := tbl.LookupMinor("release/v0.7"); got != "v0.7" {
		t.Errorf("LookupMinor: got %q want v0.7", got)
	}
	if got := tbl.LookupMinor("release/v9.9"); got != "" {
		t.Errorf("LookupMinor unknown: got %q want \"\"", got)
	}
	if got := tbl.BranchForMinor("v0.9"); got != "main" {
		t.Errorf("BranchForMinor: got %q want main", got)
	}
}

func TestParseVersionTable_NoTable(t *testing.T) {
	_, err := ParseVersionTable("# Just a heading\n\nno table here.\n")
	if err == nil {
		t.Fatal("expected error for missing table")
	}
}

const webhookVersionMD = `Rancher's Webhook follows a pre-release (v0.x) strategy of semver.

| Webhook Branch | Webhook Minor version | Rancher Minor Version | Chart Major Version |
|----------------|-----------------------|-----------------------|---------------------|
| main | v0.13 | v2.17 | 112 |
| release/v0.12 | v0.12 | v2.16 | 111 |
| release/v0.8 | v0.8 | v2.12 | 107 |
`

func TestVersionTable_LookupChartMajor(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		minor     string
		wantMajor string
		wantOK    bool
	}{
		{name: "main row", content: webhookVersionMD, minor: "v0.13", wantMajor: "112", wantOK: true},
		{name: "release row", content: webhookVersionMD, minor: "v0.12", wantMajor: "111", wantOK: true},
		{name: "oldest row", content: webhookVersionMD, minor: "v0.8", wantMajor: "107", wantOK: true},
		{name: "unknown minor", content: webhookVersionMD, minor: "v0.99", wantMajor: "", wantOK: false},
		{name: "3-column table has no chart major", content: steveVersionMD, minor: "v0.9", wantMajor: "", wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tbl, err := ParseVersionTable(tt.content)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			major, ok := tbl.LookupChartMajor(tt.minor)
			if major != tt.wantMajor || ok != tt.wantOK {
				t.Errorf("LookupChartMajor(%q) = (%q, %v), want (%q, %v)", tt.minor, major, ok, tt.wantMajor, tt.wantOK)
			}
		})
	}
}

func TestParseVersionTable_ChartMajorKeepsPair(t *testing.T) {
	tbl, err := ParseVersionTable(webhookVersionMD)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := VersionRow{Branch: "release/v0.12", Minor: "v0.12", Pair: "v2.16", ChartMajor: "111"}
	if got := tbl.Rows[1]; got != want {
		t.Errorf("row 1: got %+v want %+v", got, want)
	}
}
