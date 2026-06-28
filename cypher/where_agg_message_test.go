package cypher_test

// where_agg_message_test.go — regression gate for #1806 (sprint 253): an
// aggregate in WHERE is correctly rejected, but the error message wrongly
// referenced ORDER BY. It must name WHERE.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestAggregationInWhereMessage_1806(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	_, err := eng.Run(context.Background(), `MATCH (n) WHERE count(n) > 1 RETURN n`, nil)
	if err == nil {
		t.Fatal("aggregation in WHERE must be rejected")
	}
	if !strings.Contains(err.Error(), "WHERE") {
		t.Errorf("error should reference WHERE, got %v", err)
	}
	if strings.Contains(err.Error(), "ORDER BY") {
		t.Errorf("error must not reference ORDER BY, got %v", err)
	}
}
