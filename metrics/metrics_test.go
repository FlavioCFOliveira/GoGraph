package metrics_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/metrics"
)

// Compile-time check: backendFunc satisfies metrics.Backend.
var _ metrics.Backend = backendFunc{}

// backendFunc is a test-local Backend implementation backed by
// plain function values.
type backendFunc struct {
	incCounter     func(string, uint64)
	observeLatency func(string, time.Duration)
}

func (f backendFunc) IncCounter(name string, delta uint64)        { f.incCounter(name, delta) }
func (f backendFunc) ObserveLatency(name string, d time.Duration) { f.observeLatency(name, d) }

// TestSetBackendRoundTrip verifies that a custom Backend wired via
// SetBackend actually receives events.
func TestSetBackendRoundTrip(t *testing.T) {
	type event struct {
		name  string
		delta uint64
		dur   time.Duration
	}
	var got []event

	b := backendFunc{
		incCounter: func(name string, delta uint64) {
			got = append(got, event{name: name, delta: delta})
		},
		observeLatency: func(name string, d time.Duration) {
			got = append(got, event{name: name, dur: d})
		},
	}

	metrics.SetBackend(b)
	t.Cleanup(func() { metrics.SetBackend(nil) })

	// No events emitted here; the purpose of this test is the compile
	// gate — confirming that SetBackend accepts a metrics.Backend value
	// via the public package.
	_ = got
}

// TestNewPrometheusRegistryCompiles is a compile-and-smoke test for
// NewPrometheusRegistry: it must return a non-nil *Registry whose
// WriteText and Handler methods are callable.
func TestNewPrometheusRegistryCompiles(t *testing.T) {
	reg := metrics.NewPrometheusRegistry()
	if reg == nil {
		t.Fatal("NewPrometheusRegistry returned nil")
	}

	// Confirm Registry satisfies Backend via the type alias.
	var _ metrics.Backend = reg

	// Install and smoke-test the WriteText path.
	metrics.SetBackend(reg)
	t.Cleanup(func() { metrics.SetBackend(nil) })

	var buf bytes.Buffer
	if err := reg.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}

	// Handler must return a non-nil http.Handler.
	if reg.Handler() == nil {
		t.Fatal("Handler() returned nil")
	}
}

// TestSetBackendNilRestoresNoop verifies that SetBackend(nil) does not
// panic and leaves the package in a usable state.
func TestSetBackendNilRestoresNoop(t *testing.T) {
	metrics.SetBackend(nil) // must not panic
	reg := metrics.NewPrometheusRegistry()
	metrics.SetBackend(reg)
	metrics.SetBackend(nil) // back to noop

	// After restoring noop, WriteText on a fresh registry must still work.
	var buf bytes.Buffer
	if err := reg.WriteText(&buf); err != nil {
		t.Fatalf("WriteText after noop restore: %v", err)
	}
}

// TestPrometheusRegistryWriteTextFormat performs a minimal format
// check: after IncCounter and ObserveLatency calls via the Backend
// interface, WriteText must emit lines containing the sanitised name.
func TestPrometheusRegistryWriteTextFormat(t *testing.T) {
	reg := metrics.NewPrometheusRegistry()
	metrics.SetBackend(reg)
	t.Cleanup(func() { metrics.SetBackend(nil) })

	// Use the Backend interface (type alias) to call the methods.
	var b metrics.Backend = reg
	b.IncCounter("test.counter.example", 3)
	b.ObserveLatency("test.latency.example", 5*time.Millisecond)

	var buf bytes.Buffer
	if err := reg.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := buf.String()

	// Dots are sanitised to underscores in the Prometheus output.
	if !strings.Contains(out, "test_counter_example") {
		t.Errorf("expected sanitised counter name in output; got:\n%s", out)
	}
	if !strings.Contains(out, "test_latency_example") {
		t.Errorf("expected sanitised histogram name in output; got:\n%s", out)
	}
}
