// Command gograph-import builds a GoGraph store from CSV files, offline, at
// bulk-loader speed.
//
// It is the answer to a gap the round-3 comparative audit measured: loading
// 200 000 edges through the Cypher write path took 35 m 33 s, against 977 ms for
// Memgraph and 2.39 s for Neo4j, while the fast ingest path inside the module was
// reachable from nothing a user could call.
//
// # Why a command and not a Cypher clause
//
// The import needs exclusive ownership of the store directory: it publishes a
// whole snapshot, which under a live server would race the checkpointer and
// invalidate open readers' view. It is also not a transaction (see the durability
// contract below), so dressing it as a Cypher statement inside a session would
// promise semantics it does not have. Neo4j draws the same line — neo4j-admin
// database import is a separate offline tool from LOAD CSV.
//
// # Usage
//
//	gograph-import -store DIR -nodes nodes.csv -edges edges.csv [flags]
//
// The store directory must not exist, or must exist and be empty. Importing into
// a directory that already holds data is refused, not merged.
//
// # CSV format
//
// The nodes file has a header row. One column is the node key; every other column
// becomes a property, except columns named in -node-labels, whose values become
// labels. The edges file likewise has a header row with a source column, a target
// column, an optional type column and an optional weight column; every other
// column becomes an edge property.
//
//	nodes.csv:  id,name,age
//	            n1,Alice,30
//
//	edges.csv:  src,dst,type,since
//	            n1,n2,KNOWS,2020
//
// Property values are typed by inspection: an integer parses as an integer, a
// decimal as a float, true/false as a boolean, anything else as a string. Pass
// -string-props to disable inference and store every property as a string, which
// is the safe choice when a column holds identifiers that merely look numeric.
//
// # Durability contract
//
// The whole import is atomic and nothing else about it is. The snapshot is
// assembled under <store>/snapshot.tmp and renamed into place, and a rename within
// a directory is atomic, so the store either has no snapshot or a complete one —
// no reader can ever observe a partial import. A crash before the rename leaves
// the store exactly as it was; the partial assembly is invisible to recovery and
// removed by it.
//
// What this is NOT: a transaction. There is no transaction id, no write-ahead log
// record, no isolation level and no rollback once published — undoing an import
// means deleting the directory. There is no per-record durability acknowledgement
// and no resumption point, so a crash partway through means re-running the whole
// import. And it is concurrent with nothing: no reader, writer or checkpointer may
// touch the directory while it runs.
package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/bulkimport"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "gograph-import: %v\n", err)
		os.Exit(1)
	}
}

// config is the resolved command line.
type config struct {
	store       string
	nodesPath   string
	edgesPath   string
	keyCol      string
	srcCol      string
	dstCol      string
	typeCol     string
	weightCol   string
	labelCols   []string
	undirected  bool
	simpleGraph bool
	stringProps bool
}

// run is main's testable body: it writes progress to out and returns an error
// rather than exiting, so the whole command can be exercised in a test.
func run(args []string, out io.Writer) error {
	cfg, err := parseFlags(args)
	if err != nil {
		return err
	}

	nodes, err := readNodes(&cfg)
	if err != nil {
		return err
	}
	edges, err := readEdges(&cfg)
	if err != nil {
		return err
	}

	start := time.Now()
	res, err := bulkimport.ImportInto[int64](context.Background(), cfg.store, bulkimport.Options{
		Directed:    !cfg.undirected,
		Multigraph:  !cfg.simpleGraph,
		ExpectNodes: len(nodes),
	}, nodes, edges)
	if err != nil {
		return err
	}
	elapsed := time.Since(start)

	if _, werr := fmt.Fprintf(out, "imported %d nodes and %d edges in %v (%.2f M edges/s)\n",
		res.Stats.Nodes, res.Stats.Edges, elapsed.Round(time.Millisecond),
		float64(res.Stats.Edges)/elapsed.Seconds()/1e6); werr != nil {
		return werr
	}
	_, werr := fmt.Fprintf(out, "published %s\n", res.SnapshotDir)
	return werr
}

func parseFlags(args []string) (config, error) {
	var cfg config
	var labels string
	fs := flag.NewFlagSet("gograph-import", flag.ContinueOnError)
	fs.StringVar(&cfg.store, "store", "", "store directory to create (must be absent or empty)")
	fs.StringVar(&cfg.nodesPath, "nodes", "", "CSV file of node records")
	fs.StringVar(&cfg.edgesPath, "edges", "", "CSV file of edge records (optional)")
	fs.StringVar(&cfg.keyCol, "key", "id", "nodes column holding the node key")
	fs.StringVar(&labels, "node-labels", "", "comma-separated nodes columns whose values are labels")
	fs.StringVar(&cfg.srcCol, "src", "src", "edges column holding the source key")
	fs.StringVar(&cfg.dstCol, "dst", "dst", "edges column holding the target key")
	fs.StringVar(&cfg.typeCol, "type", "type", "edges column holding the relationship type")
	fs.StringVar(&cfg.weightCol, "weight", "weight", "edges column holding an integer weight")
	fs.BoolVar(&cfg.undirected, "undirected", false, "build an undirected graph (openCypher requires directed)")
	fs.BoolVar(&cfg.simpleGraph, "simple", false, "reject parallel edges (openCypher's model is a multigraph)")
	fs.BoolVar(&cfg.stringProps, "string-props", false, "store every property as a string, with no type inference")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if cfg.store == "" {
		return cfg, errors.New("-store is required")
	}
	if cfg.nodesPath == "" {
		return cfg, errors.New("-nodes is required")
	}
	for _, l := range strings.Split(labels, ",") {
		if l = strings.TrimSpace(l); l != "" {
			cfg.labelCols = append(cfg.labelCols, l)
		}
	}
	return cfg, nil
}

// readCSV opens path and returns its header and rows.
func readCSV(path string) (header []string, rows [][]string, err error) {
	f, err := os.Open(path) //nolint:gosec // path is an operator-supplied CLI argument
	if err != nil {
		return nil, nil, fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only file; close error is not actionable
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1 // tolerate ragged rows; missing cells are treated as absent
	recs, err := r.ReadAll()
	if err != nil {
		return nil, nil, fmt.Errorf("read %q: %w", path, err)
	}
	if len(recs) == 0 {
		return nil, nil, fmt.Errorf("%q is empty; a header row is required", path)
	}
	return recs[0], recs[1:], nil
}

// columnIndex returns the position of name in header, or -1.
func columnIndex(header []string, name string) int {
	for i, h := range header {
		if strings.EqualFold(strings.TrimSpace(h), name) {
			return i
		}
	}
	return -1
}

func readNodes(cfg *config) ([]bulkimport.Node, error) {
	header, rows, err := readCSV(cfg.nodesPath)
	if err != nil {
		return nil, err
	}
	keyIdx := columnIndex(header, cfg.keyCol)
	if keyIdx < 0 {
		return nil, fmt.Errorf("nodes file %q has no column %q (columns: %v)",
			cfg.nodesPath, cfg.keyCol, header)
	}
	labelIdx := map[int]bool{}
	for _, name := range cfg.labelCols {
		i := columnIndex(header, name)
		if i < 0 {
			return nil, fmt.Errorf("nodes file %q has no label column %q (columns: %v)",
				cfg.nodesPath, name, header)
		}
		labelIdx[i] = true
	}

	out := make([]bulkimport.Node, 0, len(rows))
	for lineNo, row := range rows {
		if keyIdx >= len(row) || strings.TrimSpace(row[keyIdx]) == "" {
			return nil, fmt.Errorf("nodes file %q line %d: empty key", cfg.nodesPath, lineNo+2)
		}
		n := bulkimport.Node{Key: row[keyIdx]}
		for i, cell := range row {
			if i == keyIdx || cell == "" {
				continue
			}
			switch {
			case labelIdx[i]:
				n.Labels = append(n.Labels, cell)
			default:
				if n.Properties == nil {
					n.Properties = make(map[string]lpg.PropertyValue, len(row)-1)
				}
				n.Properties[header[i]] = propertyValue(cell, cfg.stringProps)
			}
		}
		out = append(out, n)
	}
	return out, nil
}

func readEdges(cfg *config) ([]bulkimport.Edge[int64], error) {
	if cfg.edgesPath == "" {
		return nil, nil
	}
	header, rows, err := readCSV(cfg.edgesPath)
	if err != nil {
		return nil, err
	}
	srcIdx, dstIdx := columnIndex(header, cfg.srcCol), columnIndex(header, cfg.dstCol)
	if srcIdx < 0 || dstIdx < 0 {
		return nil, fmt.Errorf("edges file %q needs columns %q and %q (columns: %v)",
			cfg.edgesPath, cfg.srcCol, cfg.dstCol, header)
	}
	typeIdx, weightIdx := columnIndex(header, cfg.typeCol), columnIndex(header, cfg.weightCol)

	out := make([]bulkimport.Edge[int64], 0, len(rows))
	for lineNo, row := range rows {
		if srcIdx >= len(row) || dstIdx >= len(row) {
			return nil, fmt.Errorf("edges file %q line %d: missing endpoint", cfg.edgesPath, lineNo+2)
		}
		e := bulkimport.Edge[int64]{Src: row[srcIdx], Dst: row[dstIdx]}
		if typeIdx >= 0 && typeIdx < len(row) {
			e.Type = row[typeIdx]
		}
		if weightIdx >= 0 && weightIdx < len(row) && row[weightIdx] != "" {
			w, perr := strconv.ParseInt(strings.TrimSpace(row[weightIdx]), 10, 64)
			if perr != nil {
				return nil, fmt.Errorf("edges file %q line %d: weight %q is not an integer: %w",
					cfg.edgesPath, lineNo+2, row[weightIdx], perr)
			}
			e.Weight = w
		}
		for i, cell := range row {
			if i == srcIdx || i == dstIdx || i == typeIdx || i == weightIdx || cell == "" {
				continue
			}
			if e.Properties == nil {
				e.Properties = make(map[string]lpg.PropertyValue, len(row)-2)
			}
			e.Properties[header[i]] = propertyValue(cell, cfg.stringProps)
		}
		out = append(out, e)
	}
	return out, nil
}

// propertyValue types a CSV cell.
//
// Inference is convenient and occasionally wrong: an identifier column of digits
// becomes an integer, which changes how it compares and which index serves it.
// -string-props turns inference off for exactly that case, rather than leaving the
// caller to discover the coercion after the import.
func propertyValue(cell string, asString bool) lpg.PropertyValue {
	if asString {
		return lpg.StringValue(cell)
	}
	trimmed := strings.TrimSpace(cell)
	if i, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		return lpg.Int64Value(i)
	}
	if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
		return lpg.Float64Value(f)
	}
	switch strings.ToLower(trimmed) {
	case "true":
		return lpg.BoolValue(true)
	case "false":
		return lpg.BoolValue(false)
	}
	return lpg.StringValue(cell)
}
