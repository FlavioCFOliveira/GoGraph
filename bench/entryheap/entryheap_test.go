package entryheap_test

import (
	"fmt"
	"os"
	"strconv"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/bench/entryheap"
)

// envRun gates the measurement. Like the contention sweep, this is a
// measurement campaign and not a unit test: at 10M keys one run allocates
// roughly half a gigabyte and forces a dozen collections, which would blow the
// short layer's per-package budget and corrupt anything else being measured on
// the host at the time. It never runs as a side effect of `go test ./...`.
const envRun = "GOGRAPH_ENTRYHEAP"

const (
	envKeys      = "GOGRAPH_ENTRYHEAP_KEYS"
	envGCs       = "GOGRAPH_ENTRYHEAP_GCS"
	envBuild     = "GOGRAPH_ENTRYHEAP_BUILD"
	envOrder     = "GOGRAPH_ENTRYHEAP_ORDER"
	envKeepOneIn = "GOGRAPH_ENTRYHEAP_KEEP_ONE_IN"
)

func intEnv(t *testing.T, name string, def int) int {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("bad %s=%q: %v", name, raw, err)
	}
	return v
}

// TestEntryHeap measures ONE configuration and prints one machine-readable line
// per phase.
//
// RUN IT WITH -v. `go test` discards everything a PASSING package writes, so
// without -v the measurement is swallowed along with the rest.
//
// One process measures one configuration; see the package documentation for
// why looping over arms inside a single process is not a valid comparison.
func TestEntryHeap(t *testing.T) {
	if os.Getenv(envRun) == "" {
		t.Skipf("set %s=1 to run the entry-heap measurement, and pass -v or its output is discarded", envRun)
	}

	cfg := entryheap.Config{
		Keys:      intEnv(t, envKeys, 1_000_000),
		GCs:       intEnv(t, envGCs, 9),
		Build:     entryheap.BuildMode(os.Getenv(envBuild)),
		Order:     entryheap.KeyOrder(os.Getenv(envOrder)),
		KeepOneIn: intEnv(t, envKeepOneIn, 0),
	}

	samples, err := entryheap.Measure(cfg)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	for _, s := range samples {
		fmt.Printf("ENTRYHEAP phase=%s live_keys=%d bytes_per_key=%.4f objects_per_key=%.6f scan_bytes_per_key=%.4f "+
			"heap_bytes=%d heap_objects=%d gc_wall_ms_min=%.4f gc_wall_ms_med=%.4f mark_cpu_ms_min=%.4f mark_cpu_ms_med=%.4f\n",
			s.Phase, s.LiveKeys, s.BytesPerKey, s.ObjectsPerKey, s.ScanBytesPerKey,
			s.HeapObjectBytes, s.HeapObjects,
			entryheap.Min(s.GCWallMillis), entryheap.Median(s.GCWallMillis),
			entryheap.Min(s.MarkCPUMillis), entryheap.Median(s.MarkCPUMillis))
	}
}

// TestStrideIsBijection proves the strided insertion order actually visits every
// key exactly once. A stride that shared a factor with the key count would
// silently build a SMALLER index than the configuration asked for, and every
// per-key number would be divided by the wrong denominator.
//
// It is cheap and unconditional: it guards the harness, not the module.
func TestStrideIsBijection(t *testing.T) {
	for _, n := range []int{2, 3, 10, 97, 1024, 100_000, 999_983} {
		seen := make([]bool, n)
		got, err := entryheap.InsertionKeys(entryheap.Config{Keys: n, Order: entryheap.OrderStrided})
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		for step, k := range got {
			if k < 0 || int(k) >= n {
				t.Fatalf("n=%d step=%d: key %d out of range", n, step, k)
			}
			if seen[k] {
				t.Fatalf("n=%d step=%d: key %d visited twice", n, step, k)
			}
			seen[k] = true
		}
	}
}

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
