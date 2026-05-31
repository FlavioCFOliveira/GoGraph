// Example 06_csv_import — reads an edge-list CSV, builds the
// adjacency list, then writes it out as both CSV and JSON Lines.
//
// Sample output: run `go run ./examples/06_csv_import` and capture the
// stdout — the output is deterministic for the inputs hard-coded
// above and serves as the regression baseline a future change should
// preserve.
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"gograph/graph/io/csv"
	"gograph/graph/io/jsonl"
)

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run reads a small edge-list CSV into an adjacency list, then writes
// the graph back out as both CSV and JSON Lines. All output goes to w
// so a test can capture and assert it; run returns wrapped errors
// rather than terminating the process.
func run(w io.Writer) error {
	input := `# 3 example edges
alice,bob,1
bob,carol,2
carol,alice,3
`
	a, n, err := csv.ReadInto(strings.NewReader(input), csv.DefaultOptions())
	if err != nil {
		return fmt.Errorf("csv.ReadInto: %w", err)
	}
	fmt.Fprintf(w, "Ingested %d rows\n", n)

	fmt.Fprintln(w, "\nCSV out:")
	if _, err := csv.Write(w, a, csv.DefaultOptions()); err != nil {
		return fmt.Errorf("csv.Write: %w", err)
	}
	fmt.Fprintln(w, "\nJSON Lines out:")
	if _, err := jsonl.Write(w, a); err != nil {
		return fmt.Errorf("jsonl.Write: %w", err)
	}
	return nil
}
