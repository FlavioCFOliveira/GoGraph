// Example 06_csv_import — reads an edge-list CSV, builds the
// adjacency list, then writes it out as both CSV and JSON Lines.
package main

import (
	"fmt"
	"os"
	"strings"

	"gograph/graph/io/csv"
	"gograph/graph/io/jsonl"
)

func main() {
	input := `# 3 example edges
alice,bob,1
bob,carol,2
carol,alice,3
`
	a, n, err := csv.ReadInto(strings.NewReader(input), csv.DefaultOptions())
	if err != nil {
		panic(err)
	}
	fmt.Printf("Ingested %d rows\n", n)

	fmt.Println("\nCSV out:")
	_, _ = csv.Write(os.Stdout, a, csv.DefaultOptions())
	fmt.Println("\nJSON Lines out:")
	_, _ = jsonl.Write(os.Stdout, a)
}
