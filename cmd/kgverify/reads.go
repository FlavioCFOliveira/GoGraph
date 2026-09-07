package main

import (
	"fmt"
	"sort"
	"strings"
)

// symbolLabelPredicate builds the WHERE clause selecting every symbol-tier
// label, derived from symbolLabels so the query and the oracle cannot drift.
func symbolLabelPredicate(v string) string {
	labels := make([]string, 0, len(symbolLabels))
	for l := range symbolLabels {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	terms := make([]string, len(labels))
	for i, l := range labels {
		terms[i] = v + ":" + l
	}
	return strings.Join(terms, " OR ")
}

// readSymbolNodes reads every symbol-tier node.
//
// Both package-key spellings are projected: knowledge-model.md records that
// internal/sim symbols carry `package` holding the repo-relative directory
// while every other package carries `pkg` holding the import path, so reading
// only one of them makes an entire package's symbols look unplaced.
func readSymbolNodes(repo, roadmap string) ([]symNode, error) {
	q := fmt.Sprintf(`MATCH (n) WHERE %s
RETURN labels(n) AS labels, n.name AS name, n.pkg AS pkg, n.package AS pkgAlt,
       n.file AS file, n.recv AS recv, n.gitCommit AS gitCommit`, symbolLabelPredicate("n"))
	r, err := graphQuery(repo, roadmap, q)
	if err != nil {
		return nil, err
	}
	iLabels, iName := r.col("labels"), r.col("name")
	iPkg, iPkgAlt := r.col("pkg"), r.col("pkgAlt")
	iFile, iRecv, iCommit := r.col("file"), r.col("recv"), r.col("gitCommit")
	out := make([]symNode, 0, len(r.Rows))
	for _, row := range r.Rows {
		n := symNode{}
		n.Label = firstSymbolLabel(row, iLabels)
		n.Name, _ = r.str(row, iName)
		if p, ok := r.str(row, iPkg); ok {
			n.Pkg = p
		} else if p, ok := r.str(row, iPkgAlt); ok {
			n.Pkg = p
		}
		n.File, _ = r.str(row, iFile)
		n.Recv, _ = r.str(row, iRecv)
		n.GitCommit, _ = r.str(row, iCommit)
		out = append(out, n)
	}
	return out, nil
}

// firstSymbolLabel picks the symbol-tier label off a node's label list. A node
// may carry more than one label, and only the symbol-tier one is checkable
// against the tree.
func firstSymbolLabel(row []any, idx int) string {
	if idx < 0 || idx >= len(row) {
		return ""
	}
	raw, ok := row[idx].([]any)
	if !ok {
		if s, isStr := row[idx].(string); isStr {
			return s
		}
		return ""
	}
	fallback := ""
	for _, v := range raw {
		s, isStr := v.(string)
		if !isStr {
			continue
		}
		if _, isSym := symbolLabels[s]; isSym {
			return s
		}
		if fallback == "" {
			fallback = s
		}
	}
	return fallback
}

func readTaskNodes(repo, roadmap string) ([]taskNode, error) {
	const q = `MATCH (t:Task)
RETURN t.id AS id, t.status AS status, t.title AS title, t.name AS name,
       t.task_id AS taskID, t.number AS number`
	r, err := graphQuery(repo, roadmap, q)
	if err != nil {
		return nil, err
	}
	iID, iStatus, iTitle := r.col("id"), r.col("status"), r.col("title")
	iName, iLegacy, iNum := r.col("name"), r.col("taskID"), r.col("number")
	out := make([]taskNode, 0, len(r.Rows))
	for _, row := range r.Rows {
		out = append(out, taskNode{
			RawID:    at(row, iID),
			Status:   at(row, iStatus),
			Title:    at(row, iTitle),
			Name:     at(row, iName),
			LegacyID: at(row, iLegacy),
			LegacyNo: at(row, iNum),
		})
	}
	return out, nil
}

func readEdgeShapes(repo, roadmap string) ([]edgeShape, error) {
	const q = `MATCH (a)-[r]->(b)
RETURN labels(a) AS src, type(r) AS type, labels(b) AS dst, count(*) AS n`
	rows, err := graphQuery(repo, roadmap, q)
	if err != nil {
		return nil, err
	}
	iSrc, iType, iDst, iN := rows.col("src"), rows.col("type"), rows.col("dst"), rows.col("n")
	out := make([]edgeShape, 0, len(rows.Rows))
	for _, row := range rows.Rows {
		t, _ := rows.str(row, iType)
		count := 0
		if c, ok := jsonInt(at(row, iN)); ok {
			count = c
		}
		out = append(out, edgeShape{
			Src:   joinLabels(at(row, iSrc)),
			Type:  t,
			Dst:   joinLabels(at(row, iDst)),
			Count: count,
		})
	}
	return out, nil
}

func readLabelCounts(repo, roadmap string) (map[string]int, error) {
	const q = `MATCH (n) RETURN labels(n) AS labels, count(*) AS n`
	rows, err := graphQuery(repo, roadmap, q)
	if err != nil {
		return nil, err
	}
	iLabels, iN := rows.col("labels"), rows.col("n")
	out := map[string]int{}
	for _, row := range rows.Rows {
		count := 0
		if c, ok := jsonInt(at(row, iN)); ok {
			count = c
		}
		for _, l := range labelList(at(row, iLabels)) {
			out[l] += count
		}
	}
	return out, nil
}

func readComponentNodes(repo, roadmap string) ([]componentNode, error) {
	const q = `MATCH (n:Component) RETURN n.name AS name, n.path AS path, n.file AS file`
	rows, err := graphQuery(repo, roadmap, q)
	if err != nil {
		return nil, err
	}
	iName, iPath, iFile := rows.col("name"), rows.col("path"), rows.col("file")
	out := make([]componentNode, 0, len(rows.Rows))
	for _, row := range rows.Rows {
		name, _ := rows.str(row, iName)
		path, _ := rows.str(row, iPath)
		file, _ := rows.str(row, iFile)
		if name == "" {
			name = "<unnamed>"
		}
		out = append(out, componentNode{Name: name, Path: path, File: file})
	}
	return out, nil
}

// readNamedNodes looks up the fixture names across EVERY label, not only the
// symbol tier. A fabrication that comes back under a different label is still a
// fabrication, and scoping the lookup to the symbol tier would let it through.
func readNamedNodes(repo, roadmap string, names []string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, n := range names {
		if strings.ContainsAny(n, "'\"\\") {
			return nil, fmt.Errorf("fixture name %q contains a quote; fixture names must be bare Go identifiers", n)
		}
		q := fmt.Sprintf(`MATCH (n) WHERE n.name = '%s' RETURN labels(n) AS labels`, n)
		rows, err := graphQuery(repo, roadmap, q)
		if err != nil {
			return nil, err
		}
		iLabels := rows.col("labels")
		for _, row := range rows.Rows {
			out[n] = append(out[n], labelList(at(row, iLabels))...)
		}
	}
	return out, nil
}

func at(row []any, idx int) any {
	if idx < 0 || idx >= len(row) {
		return nil
	}
	return row[idx]
}

func labelList(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{t}
	default:
		return nil
	}
}

func joinLabels(v any) string {
	l := labelList(v)
	if len(l) == 0 {
		return "?"
	}
	sort.Strings(l)
	return strings.Join(l, "+")
}
