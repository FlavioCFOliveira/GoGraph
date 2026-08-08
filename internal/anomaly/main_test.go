package anomaly_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain asserts the checker's workloads leak no goroutines.
//
// It matters here because the AC5 measurement starts twelve writer and reader
// goroutines per repetition across sixty repetitions, twice: a workload that
// failed to join them would leak them by the hundred, in the package whose
// entire claim is that it observes without disturbing.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
