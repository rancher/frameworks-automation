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
