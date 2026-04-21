package config

import (
	"fmt"
	"strings"
)

// VersionTable is a parsed VERSION.md markdown table. Row order is preserved
// to match the source file. Lookups are by branch name.
type VersionTable struct {
	Rows []VersionRow
}

type VersionRow struct {
	Branch string // e.g. "main", "release/v0.7"
	Minor  string // e.g. "v0.9", "v0.7"
	// Pair is the third-column value when present (e.g. matching rancher
	// version "v2.15"). Empty for repos whose table only has 2 columns.
	Pair string
}

// ParseVersionTable extracts the first markdown table from `content` whose
// header looks like a VERSION.md table (starts with "|" and contains a
// separator row of dashes). Returns rows in source order.
//
// Recognized shapes:
//
//	| Branch | Minor | Matching X |   (3 columns; Pair populated)
//	| Branch | Minor |              (2 columns; Pair empty)
func ParseVersionTable(content string) (*VersionTable, error) {
	var (
		t       VersionTable
		inTable bool
	)
	for _, line := range strings.Split(content, "\n") {
		trim := strings.TrimSpace(line)
		if !strings.HasPrefix(trim, "|") {
			if inTable {
				break // table ended
			}
			continue
		}
		if isSeparatorRow(trim) {
			inTable = true
			continue
		}
		if !inTable {
			continue // header row, not yet inside the body
		}
		cells := splitRow(trim)
		if len(cells) < 2 {
			continue
		}
		row := VersionRow{Branch: cells[0], Minor: cells[1]}
		if len(cells) >= 3 {
			row.Pair = cells[2]
		}
		t.Rows = append(t.Rows, row)
	}
	if len(t.Rows) == 0 {
		return nil, fmt.Errorf("no VERSION.md table rows found")
	}
	return &t, nil
}

// LookupMinor returns the minor version paired to `branch`, or "" if not found.
func (t *VersionTable) LookupMinor(branch string) string {
	for _, r := range t.Rows {
		if r.Branch == branch {
			return r.Minor
		}
	}
	return ""
}

// BranchForMinor is the inverse: given a minor version, return the branch
// that ships it. Empty string if not found.
func (t *VersionTable) BranchForMinor(minor string) string {
	for _, r := range t.Rows {
		if r.Minor == minor {
			return r.Branch
		}
	}
	return ""
}

// LookupPair returns the third-column ("Pair") value for the row whose Minor
// matches `minor`. Empty string if not found or if the table has no Pair
// column. Used by the paired-dep target-computation: dep.minor -> pair.
func (t *VersionTable) LookupPair(minor string) string {
	for _, r := range t.Rows {
		if r.Minor == minor {
			return r.Pair
		}
	}
	return ""
}

// isSeparatorRow matches "|---|---|---|" with optional spaces and colons.
func isSeparatorRow(line string) bool {
	cells := splitRow(line)
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		if c == "" {
			return false
		}
		for _, r := range c {
			if r != '-' && r != ':' && r != ' ' {
				return false
			}
		}
	}
	return true
}

// splitRow splits a "| a | b | c |" row into trimmed cells.
func splitRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	parts := strings.Split(line, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}
