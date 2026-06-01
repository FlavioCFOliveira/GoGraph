package prometheus_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/metrics/prometheus"
)

// ---- helpers ----------------------------------------------------------------

func textOf(r *prometheus.Registry) string {
	var sb strings.Builder
	r.WriteText(&sb)
	return sb.String()
}

// containsLine reports whether the multi-line text output contains exactly
// the given line as a full line (not a substring).
func containsLine(text, line string) bool {
	for _, l := range strings.Split(text, "\n") {
		if l == line {
			return true
		}
	}
	return false
}

// ---- counter tests ----------------------------------------------------------

func TestRegistry_Counter(t *testing.T) {
	t.Parallel()
	r := prometheus.New()

	r.IncCounter("hits", 3)
	r.IncCounter("hits", 5)
	r.IncCounter("misses", 1)

	text := textOf(r)

	cases := []string{
		"# TYPE hits counter",
		"hits 8",
		"# TYPE misses counter",
		"misses 1",
	}
	for _, want := range cases {
		if !containsLine(text, want) {
			t.Errorf("WriteText output missing line %q\nFull output:\n%s", want, text)
		}
	}
}

func TestRegistry_CounterZero(t *testing.T) {
	t.Parallel()
	r := prometheus.New()
	r.IncCounter("c", 0)
	text := textOf(r)
	if !containsLine(text, "c 0") {
		t.Errorf("expected 'c 0' in output; got:\n%s", text)
	}
}

// ---- histogram tests --------------------------------------------------------

func TestRegistry_Histogram(t *testing.T) {
	t.Parallel()
	r := prometheus.New()

	// One 200 µs observation → lands in the 500 µs bucket (index 1).
	// One 2 ms observation  → lands in the 5 ms bucket  (index 3).
	obs1 := 200 * time.Microsecond
	obs2 := 2 * time.Millisecond
	r.ObserveLatency("lat", obs1)
	r.ObserveLatency("lat", obs2)

	text := textOf(r)

	// Cumulative bucket contents:
	// le=100µs  (0.0001): 0  (obs1 > 100µs)
	// le=500µs  (0.0005): 1  (obs1 <= 500µs)
	// le=1ms    (0.001):  1
	// le=5ms    (0.005):  2  (obs2 <= 5ms)
	// le=10ms   (0.01):   2
	// ... rest  (0.05, 0.1, 0.5, 1, 5): 2
	// +Inf: 2
	//
	// Labels use %g formatting (seconds), which for these sub-ms values
	// produces fixed-point form: 0.0001, 0.0005, etc.
	bucketCases := []struct {
		le    string
		count int
	}{
		{"0.0001", 0},
		{"0.0005", 1},
		{"0.001", 1},
		{"0.005", 2},
		{"0.01", 2},
		{"0.05", 2},
		{"0.1", 2},
		{"0.5", 2},
		{"1", 2},
		{"5", 2},
	}
	for _, tc := range bucketCases {
		want := fmt.Sprintf("lat_bucket{le=%q} %d", tc.le, tc.count)
		if !containsLine(text, want) {
			t.Errorf("missing line %q\nFull output:\n%s", want, text)
		}
	}

	if !containsLine(text, "lat_bucket{le=\"+Inf\"} 2") {
		t.Errorf("missing +Inf bucket line\nFull output:\n%s", text)
	}
	if !containsLine(text, "lat_count 2") {
		t.Errorf("missing _count line\nFull output:\n%s", text)
	}

	// Verify _sum: obs1 + obs2 in seconds.
	expectedSumSec := obs1.Seconds() + obs2.Seconds()
	wantSum := fmt.Sprintf("lat_sum %g", expectedSumSec)
	if !containsLine(text, wantSum) {
		t.Errorf("missing _sum line %q\nFull output:\n%s", wantSum, text)
	}

	if !containsLine(text, "# TYPE lat histogram") {
		t.Errorf("missing TYPE line\nFull output:\n%s", text)
	}
}

func TestRegistry_HistogramOverflow(t *testing.T) {
	t.Parallel()
	r := prometheus.New()

	// Observation beyond the largest bucket (5 s) goes only to +Inf.
	r.ObserveLatency("big", 10*time.Second)
	text := textOf(r)

	// All finite buckets must be 0. Labels use %g seconds formatting.
	for _, le := range []string{"0.0001", "0.0005", "0.001", "0.005", "0.01", "0.05", "0.1", "0.5", "1", "5"} {
		want := fmt.Sprintf("big_bucket{le=%q} 0", le)
		if !containsLine(text, want) {
			t.Errorf("missing zero bucket %q\nFull output:\n%s", want, text)
		}
	}
	if !containsLine(text, "big_bucket{le=\"+Inf\"} 1") {
		t.Errorf("missing +Inf bucket\nFull output:\n%s", text)
	}
	if !containsLine(text, "big_count 1") {
		t.Errorf("missing _count\nFull output:\n%s", text)
	}
}

// ---- concurrency test -------------------------------------------------------

func TestRegistry_Concurrent(t *testing.T) {
	t.Parallel()

	const goroutines = 100
	const ops = 1000

	r := prometheus.New()
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			name := fmt.Sprintf("worker_%d", id%5) // 5 distinct names → contention
			for j := 0; j < ops; j++ {
				r.IncCounter(name, 1)
				r.ObserveLatency(name, time.Duration(j)*time.Microsecond)
			}
		}(i)
	}

	wg.Wait()

	// Drain the output — must not panic and must contain recognisable output.
	text := textOf(r)
	if !strings.Contains(text, "# TYPE") {
		t.Errorf("unexpected empty output after concurrent writes:\n%s", text)
	}

	// Each of the 5 names was written by goroutines/5 = 20 goroutines × ops times.
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("worker_%d", i)
		want := fmt.Sprintf("%s %d", name, goroutines/5*ops)
		if !containsLine(text, want) {
			t.Errorf("counter %s: expected %q in output\nFull:\n%s", name, want, text)
		}
	}
}

// ---- HTTP handler tests -----------------------------------------------------

func TestRegistry_Handler(t *testing.T) {
	t.Parallel()
	r := prometheus.New()
	r.IncCounter("req.total", 42)

	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics") //nolint:gosec // test-only URL
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; version=0.0.4; charset=utf-8", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	text := string(body)

	if !containsLine(text, "req_total 42") {
		t.Errorf("body missing 'req_total 42'\nBody:\n%s", text)
	}
}

// ---- name sanitization tests ------------------------------------------------

func TestRegistry_SanitizeName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  string
	}{
		{"store.wal.Write", "store_wal_Write"},
		{"search/dijkstra", "search_dijkstra"},
		{"cache-hits", "cache_hits"},
		{"a.b/c-d", "a_b_c_d"},
		{"plain", "plain"},
	}

	for _, tc := range cases {
		r := prometheus.New()
		r.IncCounter(tc.input, 7)
		text := textOf(r)
		want := fmt.Sprintf("%s 7", tc.want)
		if !containsLine(text, want) {
			t.Errorf("name %q: expected %q in output\nFull:\n%s", tc.input, want, text)
		}
		// The original unsanitized name must not appear.
		if tc.input != tc.want && strings.Contains(text, tc.input) {
			t.Errorf("name %q: unsanitized name leaked into output\nFull:\n%s", tc.input, text)
		}
	}
}

// TestRegistry_SanitizeHistogram verifies sanitization for ObserveLatency.
func TestRegistry_SanitizeHistogram(t *testing.T) {
	t.Parallel()
	r := prometheus.New()
	r.ObserveLatency("search/BFS", time.Millisecond)
	text := textOf(r)
	if !containsLine(text, "# TYPE search_BFS histogram") {
		t.Errorf("sanitized histogram TYPE line missing\nFull:\n%s", text)
	}
	if strings.Contains(text, "search/BFS") {
		t.Errorf("unsanitized name leaked into histogram output\nFull:\n%s", text)
	}
}
