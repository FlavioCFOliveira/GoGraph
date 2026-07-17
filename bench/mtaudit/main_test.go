package mtaudit

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

// TestMain silences the engine's slog output so the benchmark stream stays
// benchstat-parseable. The only log line these benchmarks emit is the
// non-multigraph construction warning, raised inside cypher.NewEngine — outside
// every b.ResetTimer region — so discarding it changes no measured number, only
// the noise interleaved with the results. NOT part of the module.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}
