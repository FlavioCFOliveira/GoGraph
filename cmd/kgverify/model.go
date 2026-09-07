package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// edgeForm is one documented endpoint shape for an edge type.
type edgeForm struct {
	Src  string
	Type string
	Dst  string
}

// model is what knowledge-model.md declares the graph's schema to be: the set
// of node labels it documents, and the set of edge endpoint shapes it admits.
//
// The document is the schema of record, so the checks derive their expectations
// from it rather than from a list hard-coded here. Documenting a label or an
// edge form is therefore how you make the checker accept it — which is the
// point: the model and the graph cannot drift apart silently.
type model struct {
	labels    map[string]struct{}
	edgeTypes map[string]struct{}
	edgeForms map[edgeForm]struct{}
}

var (
	labelCell  = regexp.MustCompile("^\\s*`([A-Za-z][A-Za-z0-9_]*)`\\s*$")
	etypeCell  = regexp.MustCompile("^\\s*`([A-Z][A-Z0-9_]*)`\\s*$")
	formInCell = regexp.MustCompile(`\(([A-Za-z\\|]+)\)-\[:([A-Z][A-Z0-9_]*)\]->\(([A-Za-z\\|]+)\)`)
)

// splitRow splits a Markdown table row into cells, honouring \| escapes.
func splitRow(line string) []string {
	var cells []string
	var cur strings.Builder
	esc := false
	for _, r := range line {
		switch {
		case esc:
			if r != '|' {
				cur.WriteRune('\\')
			}
			cur.WriteRune(r)
			esc = false
		case r == '\\':
			esc = true
		case r == '|':
			cells = append(cells, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if esc {
		cur.WriteRune('\\')
	}
	cells = append(cells, cur.String())
	return cells
}

// sectionLines returns the lines that follow the given heading, up to the next
// heading of any level. Stopping at any '#' matters: the "Counts by label"
// sub-table also holds backticked label names, and running past the heading
// folds those stale counts into the documented set.
func sectionLines(lines []string, heading string) []string {
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == heading {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}
	var out []string
	for _, l := range lines[start+1:] {
		if strings.HasPrefix(l, "#") {
			break
		}
		out = append(out, l)
	}
	return out
}

func parseModel(path string) (*model, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is the repo's own knowledge-model.md, resolved from the repo root.
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	lines := strings.Split(string(raw), "\n")
	m := &model{
		labels:    make(map[string]struct{}),
		edgeTypes: make(map[string]struct{}),
		edgeForms: make(map[edgeForm]struct{}),
	}
	for _, l := range sectionLines(lines, "## Node labels") {
		if !strings.HasPrefix(l, "|") {
			continue
		}
		cells := splitRow(l)
		if len(cells) < 2 {
			continue
		}
		if mm := labelCell.FindStringSubmatch(cells[1]); mm != nil {
			m.labels[mm[1]] = struct{}{}
		}
	}
	for _, l := range sectionLines(lines, "## Edge types") {
		if !strings.HasPrefix(l, "|") {
			continue
		}
		cells := splitRow(l)
		if len(cells) < 3 {
			continue
		}
		mm := etypeCell.FindStringSubmatch(cells[1])
		if mm == nil {
			continue
		}
		m.edgeTypes[mm[1]] = struct{}{}
		for _, f := range formInCell.FindAllStringSubmatch(cells[2], -1) {
			for _, a := range strings.Split(strings.ReplaceAll(f[1], `\|`, "|"), "|") {
				for _, b := range strings.Split(strings.ReplaceAll(f[3], `\|`, "|"), "|") {
					if a == "" || b == "" {
						continue
					}
					m.edgeForms[edgeForm{Src: a, Type: f[2], Dst: b}] = struct{}{}
				}
			}
		}
	}
	return m, nil
}

func (m *model) hasLabel(l string) bool {
	_, ok := m.labels[l]
	return ok
}

func (m *model) hasEdgeType(t string) bool {
	_, ok := m.edgeTypes[t]
	return ok
}

func (m *model) hasEdgeForm(f edgeForm) bool {
	_, ok := m.edgeForms[f]
	return ok
}
