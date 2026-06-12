package metrics_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/FlavioCFOliveira/GoGraph/metrics"
)

// ExampleSetBackend demonstrates the canonical wire-up documented in
// docs/metrics.md. This example compiles and runs as part of the
// normal test suite, serving as the compile-check gate for the public
// metrics facade.
func ExampleSetBackend() {
	// Create a Prometheus-compatible registry (no external dependencies).
	reg := metrics.NewPrometheusRegistry()

	// Install it as the global backend for all GoGraph blocking APIs.
	metrics.SetBackend(reg)
	defer metrics.SetBackend(nil) // restore noop after the example

	// Expose the /metrics endpoint.
	mux := http.NewServeMux()
	mux.Handle("/metrics", reg.Handler())

	// In production: http.ListenAndServe(":9090", mux)
	// Here we use a test server to show the endpoint is reachable.
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	fmt.Println(resp.StatusCode)
	// Output:
	// 200
}
