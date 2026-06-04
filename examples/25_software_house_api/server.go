package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Server is the long-running HTTP front-end over a single dataStore. It
// owns the *http.Server and orchestrates graceful shutdown.
//
// Concurrency: the Cypher engine's read execution is lock-free over an
// immutable snapshot, but its plan- and filter-building phase reads the
// live adjacency offsets and interning tables, which a concurrent write
// mutates. The serialisation that makes this safe lives in the dataStore,
// not the Server: every handler enters through dataStore.acquire (a shared
// hold for readers, an exclusive hold for writers), held across the whole
// engine call. Because Close takes the same exclusive hold, the store owns
// the complete read/write/close contract and a write can never be
// mid-commit when the WAL is released.
type Server struct {
	ds   *dataStore
	http *http.Server
}

// newServer wires the routes and configures a bounded *http.Server. All
// timeouts and the header-size limit are set explicitly so a slow or
// abusive client cannot exhaust server resources.
func newServer(ds *dataStore, addr string) *Server {
	s := &Server{ds: ds}

	mux := http.NewServeMux()
	// Method-qualified patterns (Go 1.22+): a path match with the wrong
	// method yields 405 Method Not Allowed automatically.
	mux.HandleFunc("POST /query", s.handleQuery)
	mux.HandleFunc("POST /seed", s.handleSeed)
	mux.HandleFunc("GET /stats", s.handleStats)
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	s.http = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16, // 64 KiB of request headers
	}
	return s
}

// ListenAndServe starts serving and blocks until the server is shut down.
// A clean shutdown (http.ErrServerClosed) is reported as nil.
func (s *Server) ListenAndServe() error {
	if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown gracefully drains in-flight requests, then flushes durable
// state. The ordering — drain, snapshot, close WAL — lets in-flight clients
// receive their responses and builds the final snapshot from a quiescent
// graph. Crash-safety no longer depends on this ordering: dataStore.Close
// takes the store's exclusive hold itself, so it quiesces any straggler
// write before releasing the WAL even if shutdown ordering changes. The
// snapshot is an optimisation; durability is already guaranteed at each
// commit.
func (s *Server) Shutdown(ctx context.Context) error {
	var errs []error
	if err := s.http.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("http shutdown: %w", err))
	}
	if err := s.ds.snapshotNow(); err != nil {
		errs = append(errs, fmt.Errorf("final snapshot: %w", err))
	}
	if err := s.ds.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close store: %w", err))
	}
	return errors.Join(errs...)
}

// handleHealthz is a trivial liveness probe. It does not touch the graph.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
}
