package explain

import (
	"context"
	"sync"
	"testing"
	"time"

	"gograph/cypher/exec"
	"gograph/cypher/expr"
)

// ─────────────────────────────────────────────────────────────────────────────
// TestDbHitsCounter_Basic
// ─────────────────────────────────────────────────────────────────────────────

func TestDbHitsCounter_Basic(t *testing.T) {
	var c DbHitsCounter

	if got := c.Load(); got != 0 {
		t.Fatalf("initial Load() = %d, want 0", got)
	}

	c.Add(5)
	if got := c.Load(); got != 5 {
		t.Fatalf("after Add(5): Load() = %d, want 5", got)
	}

	c.Add(3)
	if got := c.Load(); got != 8 {
		t.Fatalf("after Add(3): Load() = %d, want 8", got)
	}

	c.Reset()
	if got := c.Load(); got != 0 {
		t.Fatalf("after Reset(): Load() = %d, want 0", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestInstrumentedScan_CountsHits
// ─────────────────────────────────────────────────────────────────────────────

func TestInstrumentedScan_CountsHits(t *testing.T) {
	const rowCount = 37
	src := &sliceSource{total: rowCount}
	var counter DbHitsCounter
	is := NewInstrumentedScan(src, &counter)

	n := drainOperator(t, is)

	if n != rowCount {
		t.Fatalf("drained %d rows, want %d", n, rowCount)
	}
	if got := counter.Load(); got != uint64(rowCount) {
		t.Fatalf("counter = %d, want %d", got, rowCount)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestDbHitsCounter_Concurrent
// ─────────────────────────────────────────────────────────────────────────────

func TestDbHitsCounter_Concurrent(t *testing.T) {
	const goroutines = 100
	const addsPerGoroutine = 1000
	const want = goroutines * addsPerGoroutine

	var c DbHitsCounter
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range addsPerGoroutine {
				c.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := c.Load(); got != want {
		t.Fatalf("counter = %d, want %d", got, want)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// BenchmarkInstrumentedScan_Overhead — counter overhead < 5%
// ─────────────────────────────────────────────────────────────────────────────

const benchRows = 10_000

// rawScanSink drains a sliceSource without instrumentation and returns elapsed.
func rawScanSink(b *testing.B, rows int) time.Duration {
	b.Helper()
	src := &sliceSource{total: rows}
	if err := src.Init(context.Background()); err != nil {
		b.Fatal(err)
	}
	start := time.Now()
	var row exec.Row
	for {
		ok, err := src.Next(&row)
		if err != nil {
			b.Fatal(err)
		}
		if !ok {
			break
		}
	}
	elapsed := time.Since(start)
	if err := src.Close(); err != nil {
		b.Fatal(err)
	}
	return elapsed
}

// instrumentedScanSink drains an InstrumentedScan and returns elapsed.
func instrumentedScanSink(b *testing.B, rows int) time.Duration {
	b.Helper()
	src := &sliceSource{total: rows}
	var counter DbHitsCounter
	is := NewInstrumentedScan(src, &counter)
	if err := is.Init(context.Background()); err != nil {
		b.Fatal(err)
	}
	start := time.Now()
	var row exec.Row
	for {
		ok, err := is.Next(&row)
		if err != nil {
			b.Fatal(err)
		}
		if !ok {
			break
		}
	}
	elapsed := time.Since(start)
	if err := is.Close(); err != nil {
		b.Fatal(err)
	}
	return elapsed
}

// BenchmarkInstrumentedScan_Raw benchmarks a raw scan for comparison.
func BenchmarkInstrumentedScan_Raw(b *testing.B) {
	for b.Loop() {
		rawScanSink(b, benchRows)
	}
}

// BenchmarkInstrumentedScan_Instrumented benchmarks an instrumented scan.
func BenchmarkInstrumentedScan_Instrumented(b *testing.B) {
	for b.Loop() {
		instrumentedScanSink(b, benchRows)
	}
}

// overheadRows is larger than benchRows so raw scan duration exceeds 1 ms,
// keeping relative OS jitter well below the 25 % threshold.
const overheadRows = 200_000

// TestInstrumentedScan_OverheadBelow25Pct verifies at runtime that instrumented
// scan overhead stays under 25 % relative to the raw scan on 200k rows.
//
// Using 200k rows ensures the raw scan lasts ≥1 ms, making OS scheduling jitter
// negligible relative to the 25 % budget. The test is skipped under -race (atomic
// ops carry extra cost) and in -short mode.
func TestInstrumentedScan_OverheadBelow25Pct(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping overhead test in short mode")
	}
	if raceEnabled {
		t.Skip("skipping overhead test under race detector (-race inflates atomic costs)")
	}

	// Warmup: one unmetered pass to prime caches.
	measureOverheadRaw()
	measureOverheadInstrumented()

	const trials = 20
	var rawTotal, instTotal time.Duration
	for range trials {
		rawTotal += measureOverheadRaw()
		instTotal += measureOverheadInstrumented()
	}
	rawAvg := rawTotal / trials
	instAvg := instTotal / trials

	if rawAvg < time.Millisecond {
		t.Skip("raw scan < 1 ms — clock resolution too coarse for this platform")
	}

	overhead := float64(instAvg-rawAvg) / float64(rawAvg) * 100
	if overhead > 25.0 {
		t.Fatalf("instrumented scan overhead %.1f%% exceeds 25%% (raw=%v inst=%v)",
			overhead, rawAvg, instAvg)
	}
	t.Logf("dbHits overhead: %.2f%% (raw=%v inst=%v)", overhead, rawAvg, instAvg)
}

func measureOverheadRaw() time.Duration {
	src := &sliceSource{total: overheadRows}
	_ = src.Init(context.Background())
	var row exec.Row
	start := time.Now()
	for {
		ok, err := src.Next(&row)
		if err != nil || !ok {
			break
		}
	}
	d := time.Since(start)
	_ = src.Close()
	return d
}

func measureOverheadInstrumented() time.Duration {
	src := &sliceSource{total: overheadRows}
	var counter DbHitsCounter
	is := NewInstrumentedScan(src, &counter)
	_ = is.Init(context.Background())
	var row exec.Row
	start := time.Now()
	for {
		ok, err := is.Next(&row)
		if err != nil || !ok {
			break
		}
	}
	d := time.Since(start)
	_ = is.Close()
	return d
}


// ─────────────────────────────────────────────────────────────────────────────
// Compile-time interface checks
// ─────────────────────────────────────────────────────────────────────────────

var (
	_ exec.Operator = (*InstrumentedScan)(nil)
	_ exec.Operator = (*ProfiledOperator)(nil)
)

// valueCheck ensures expr package is used.
var _ expr.Value = expr.IntegerValue(0)
