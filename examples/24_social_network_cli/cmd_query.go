package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// cmdQuery runs a single Cypher query against the data directory and
// emits each result row as a JSON Lines record on stdout.
//
// The query is read from the first positional argument, or from the
// entire stdin stream when no positional is given. The query is run
// through Engine.RunInTx so the same code path handles read and write
// queries (the WAL fsyncs at commit when the query mutates state).
//
// On a Cypher error, cmdQuery returns the wrapped error so main maps it
// to exit code 1 and prints the diagnostic on stderr.
func cmdQuery(args []string) error {
	dir, rest, err := parseDataDir("query", args)
	if err != nil {
		return err
	}
	query, err := readQuery(rest, os.Stdin)
	if err != nil {
		return err
	}
	ctx := context.Background()
	return runQuery(ctx, dir, query, os.Stdout)
}

// readQuery returns the Cypher text either from the first positional
// argument or, if none is given, from r (typically os.Stdin). Empty
// queries are rejected as usage errors so callers see a clear message
// instead of an opaque parser error.
func readQuery(positional []string, r io.Reader) (string, error) {
	if len(positional) > 0 {
		q := strings.TrimSpace(positional[0])
		if q == "" {
			return "", newUsageError("query: empty positional argument")
		}
		return q, nil
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("query: read stdin: %w", err)
	}
	q := strings.TrimSpace(string(raw))
	if q == "" {
		return "", newUsageError("query: no query supplied (positional argument or stdin required)")
	}
	return q, nil
}

// runQuery executes the Cypher query against the data directory at dir
// and emits each record to out. It is split out from cmdQuery so the
// round-trip test in T9 can drive it with a captured *bytes.Buffer
// instead of os.Stdout.
func runQuery(ctx context.Context, dir, query string, out io.Writer) (retErr error) {
	o, err := openStore(ctx, dir)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := o.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("query: close store: %w", cerr)
		}
	}()

	res, err := o.engine.RunInTx(ctx, query, nil)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	defer func() {
		if cerr := res.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("query: close result: %w", cerr)
		}
	}()

	for res.Next() {
		rec := res.Record()
		// Write-only Cypher statements (CREATE / SET / DELETE without a
		// RETURN clause) produce one synthetic, empty-column row used by
		// the engine to drive the write pipeline. Skip it so the JSONL
		// stream remains a faithful "rows" view of the query.
		if len(rec) == 0 {
			continue
		}
		if err := writeRecord(out, rec); err != nil {
			return fmt.Errorf("query: write record: %w", err)
		}
	}
	if err := res.Err(); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("query: %w", err)
		}
		return fmt.Errorf("query: iterate: %w", err)
	}
	return nil
}
