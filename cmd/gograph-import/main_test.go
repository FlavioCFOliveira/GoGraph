package main

// main_test.go — rmp #2180.
//
// The acceptance criterion is that a user can trigger a bulk import WITHOUT
// dropping to the Go API. These tests drive the command's own body — flag
// parsing, CSV reading, type inference, the import — and then reopen the store
// through recovery, so what is verified is the user-facing path end to end rather
// than the library underneath it.
//
// Layer: short.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/store/bulkimport"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// writeFile writes content to dir/name and returns the path.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

const (
	nodesCSV = `id,name,age,kind,active
n1,Alice,30,Person,true
n2,Bob,25,Person,false
n3,Carol,41,Person,true
`
	edgesCSV = `src,dst,type,weight,since
n1,n2,KNOWS,7,2020
n2,n3,KNOWS,3,2021
n1,n3,FOLLOWS,1,2022
`
)

func openStore(t *testing.T, dir string) recovery.Result[string, int64] {
	t.Helper()
	res, err := recovery.Open[string, int64](dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open(%q): %v", dir, err)
	}
	return res
}

// TestRun_ImportsFromCSVAndOpens is the acceptance test: CSV in, openable store
// out, with labels and typed properties intact.
func TestRun_ImportsFromCSVAndOpens(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	nodes := writeFile(t, work, "nodes.csv", nodesCSV)
	edges := writeFile(t, work, "edges.csv", edgesCSV)
	store := filepath.Join(work, "store")

	var out bytes.Buffer
	if err := run([]string{
		"-store", store, "-nodes", nodes, "-edges", edges, "-node-labels", "kind",
	}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "imported 3 nodes and 3 edges") {
		t.Fatalf("output does not report what was imported: %q", out.String())
	}

	res := openStore(t, store)
	if !res.SnapshotHit {
		t.Fatal("the imported store did not open from its snapshot")
	}
	if res.WALOps != 0 {
		t.Fatalf("WALOps = %d, want 0", res.WALOps)
	}
	g := res.Graph
	if got := g.LiveOrder(); got != 3 {
		t.Fatalf("LiveOrder = %d, want 3", got)
	}

	// The label column became a label, not a property.
	labels := g.NodeLabels("n1")
	if len(labels) != 1 || labels[0] != "Person" {
		t.Fatalf("n1 labels = %v, want [Person]", labels)
	}
	if _, ok := g.GetNodeProperty("n1", "kind"); ok {
		t.Fatal("the label column was also stored as a property")
	}

	// Type inference: integer, string and boolean each land as their own kind.
	age, ok := g.GetNodeProperty("n1", "age")
	if !ok {
		t.Fatal("n1 lost property age")
	}
	if got, _ := age.Int64(); got != 30 {
		t.Fatalf("n1.age = %v, want integer 30", got)
	}
	name, ok := g.GetNodeProperty("n1", "name")
	if !ok {
		t.Fatal("n1 lost property name")
	}
	if got, _ := name.String(); got != "Alice" {
		t.Fatalf("n1.name = %q, want Alice", got)
	}
	active, ok := g.GetNodeProperty("n1", "active")
	if !ok {
		t.Fatal("n1 lost property active")
	}
	if got, _ := active.Bool(); got != true {
		t.Fatalf("n1.active = %v, want boolean true", got)
	}
}

// TestRun_RefusesNonEmptyStore pins that the command surfaces the library's
// precondition rather than swallowing it: importing over existing data must fail
// loudly, because this path writes no WAL and recovery would replay a
// pre-existing one on top of the new snapshot.
func TestRun_RefusesNonEmptyStore(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	nodes := writeFile(t, work, "nodes.csv", nodesCSV)
	store := filepath.Join(work, "store")
	if err := os.MkdirAll(store, 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, store, "wal", "frames")

	err := run([]string{"-store", store, "-nodes", nodes}, &bytes.Buffer{})
	if !errors.Is(err, bulkimport.ErrStoreNotEmpty) {
		t.Fatalf("run into a store holding a WAL = %v, want ErrStoreNotEmpty", err)
	}
}

// TestRun_StringPropsDisablesInference pins the escape hatch. An identifier column
// of digits becomes an integer under inference, which changes how it compares and
// which index serves it; -string-props is how a caller says the column is text.
func TestRun_StringPropsDisablesInference(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	nodes := writeFile(t, work, "nodes.csv", "id,zip\nn1,01234\n")
	store := filepath.Join(work, "store")

	if err := run([]string{"-store", store, "-nodes", nodes, "-string-props"},
		&bytes.Buffer{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	g := openStore(t, store).Graph
	zip, ok := g.GetNodeProperty("n1", "zip")
	if !ok {
		t.Fatal("n1 lost property zip")
	}
	if got, isStr := zip.String(); !isStr || got != "01234" {
		t.Fatalf("n1.zip = %v (string=%v), want the string 01234 — inference was not disabled",
			got, isStr)
	}
}

// TestRun_MissingColumnsAreReportedWithTheHeader pins that a misconfigured column
// name names the columns that DO exist, so the operator can fix it in one step.
func TestRun_MissingColumnsAreReportedWithTheHeader(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	nodes := writeFile(t, work, "nodes.csv", "key,name\nn1,Alice\n")
	store := filepath.Join(work, "store")

	err := run([]string{"-store", store, "-nodes", nodes}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("a missing key column was accepted")
	}
	msg := err.Error()
	for _, want := range []string{`no column "id"`, "key", "name"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not mention %q; the operator cannot see the real columns", msg, want)
		}
	}
}

// TestRun_RequiredFlags pins the two flags without which nothing can happen.
func TestRun_RequiredFlags(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no store", []string{"-nodes", "x.csv"}},
		{"no nodes", []string{"-store", "s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := run(tc.args, &bytes.Buffer{}); err == nil {
				t.Fatalf("run(%v) succeeded, want a required-flag error", tc.args)
			}
		})
	}
}

// TestRun_UnknownEdgeEndpointIsReported pins that the library's refusal to invent
// endpoints reaches the user with the offending key named, since a typo in an
// edge file is the likeliest real mistake.
func TestRun_UnknownEdgeEndpointIsReported(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	nodes := writeFile(t, work, "nodes.csv", "id\nn1\nn2\n")
	edges := writeFile(t, work, "edges.csv", "src,dst\nn1,typo\n")
	store := filepath.Join(work, "store")

	err := run([]string{"-store", store, "-nodes", nodes, "-edges", edges}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("an edge to an unknown key was accepted")
	}
	if !strings.Contains(err.Error(), "typo") {
		t.Fatalf("error %q does not name the offending key", err)
	}
}

// TestRun_NodesOnlyImport pins that -edges is genuinely optional.
func TestRun_NodesOnlyImport(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	nodes := writeFile(t, work, "nodes.csv", nodesCSV)
	store := filepath.Join(work, "store")

	var out bytes.Buffer
	if err := run([]string{"-store", store, "-nodes", nodes}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "imported 3 nodes and 0 edges") {
		t.Fatalf("output = %q, want 3 nodes and 0 edges", out.String())
	}
	if got := openStore(t, store).Graph.LiveOrder(); got != 3 {
		t.Fatalf("LiveOrder = %d, want 3", got)
	}
}

// TestPropertyValue_Inference pins the inference table directly, so a change to it
// is a deliberate act rather than a side effect.
func TestPropertyValue_Inference(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		kind string
	}{
		{"42", "int"},
		{"-42", "int"},
		{"4.5", "float"},
		{"1e3", "float"},
		{"true", "bool"},
		{"FALSE", "bool"},
		{"Alice", "string"},
		{"", "string"},
		{"007", "int"}, // documented consequence: a leading zero still parses
	} {
		t.Run(tc.in+"/"+tc.kind, func(t *testing.T) {
			v := propertyValue(tc.in, false)
			var got string
			switch {
			case func() bool { _, ok := v.Int64(); return ok }():
				got = "int"
			case func() bool { _, ok := v.Float64(); return ok }():
				got = "float"
			case func() bool { _, ok := v.Bool(); return ok }():
				got = "bool"
			default:
				got = "string"
			}
			if got != tc.kind {
				t.Fatalf("propertyValue(%q) inferred %s, want %s", tc.in, got, tc.kind)
			}
			// With inference off, everything is a string.
			if _, isStr := propertyValue(tc.in, true).String(); !isStr {
				t.Fatalf("propertyValue(%q, asString) is not a string", tc.in)
			}
		})
	}
}

// TestRun_LargeImportReportsARate exercises the path at a size where the reported
// rate is meaningful, and doubles as a smoke test that nothing in the CSV reader
// is quadratic.
func TestRun_LargeImportReportsARate(t *testing.T) {
	t.Parallel()
	const n = 2000
	var nb, eb strings.Builder
	nb.WriteString("id,name\n")
	for i := 0; i < n; i++ {
		nb.WriteString("k" + strconv.Itoa(i) + ",name" + strconv.Itoa(i) + "\n")
	}
	eb.WriteString("src,dst,type\n")
	for i := 0; i < n; i++ {
		eb.WriteString("k" + strconv.Itoa(i) + ",k" + strconv.Itoa((i*7+1)%n) + ",KNOWS\n")
	}
	work := t.TempDir()
	nodes := writeFile(t, work, "nodes.csv", nb.String())
	edges := writeFile(t, work, "edges.csv", eb.String())
	store := filepath.Join(work, "store")

	var out bytes.Buffer
	if err := run([]string{"-store", store, "-nodes", nodes, "-edges", edges}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "M edges/s") {
		t.Fatalf("output does not report a rate: %q", out.String())
	}
	if got := openStore(t, store).Graph.LiveOrder(); got != n {
		t.Fatalf("LiveOrder = %d, want %d", got, n)
	}
	t.Logf("%s", strings.TrimSpace(out.String()))
}
