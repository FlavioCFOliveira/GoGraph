package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestConcurrentReadersAndWriters hammers the server with many concurrent
// readers and writers. Run under -race it asserts the server has no data
// race; the final Developer count asserts every serialized write applied
// exactly once while reads ran in parallel. goleak (TestMain) confirms no
// goroutine leaked.
func TestConcurrentReadersAndWriters(t *testing.T) {
	ts, c := newTestServer(t, true)

	const nReaders, nWriters, iters = 16, 8, 15

	// reqStatus issues a request and returns the status code; it is
	// goroutine-safe (no t.Fatal), reporting failures via t.Errorf.
	reqStatus := func(method, path, body string) (int, []byte, error) {
		var r io.Reader
		if body != "" {
			r = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, ts.URL+path, r)
		if err != nil {
			return 0, nil, err
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw, nil
	}

	var wg sync.WaitGroup

	// Readers: /stats and a read query, repeatedly.
	for i := 0; i < nReaders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				if st, raw, err := reqStatus(http.MethodGet, "/stats", ""); err != nil || st != http.StatusOK {
					t.Errorf("reader /stats: status=%d err=%v body=%s", st, err, raw)
					return
				}
				const q = `{"query":"MATCH (c:Component) RETURN count(c) AS n"}`
				if st, raw, err := reqStatus(http.MethodPost, "/query", q); err != nil || st != http.StatusOK {
					t.Errorf("reader /query: status=%d err=%v body=%s", st, err, raw)
					return
				}
			}
		}()
	}

	// Writers: each creates distinct Developer nodes (no key collisions),
	// so the total number of successful writes is deterministic.
	var writes int64
	for i := 0; i < nWriters; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				key := fmt.Sprintf("dev:w%d_%d", id, j)
				body := fmt.Sprintf(`{"query":"CREATE (d:Developer:People {key:'%s'})"}`, key)
				if st, raw, err := reqStatus(http.MethodPost, "/query", body); err != nil || st != http.StatusOK {
					t.Errorf("writer /query: status=%d err=%v body=%s", st, err, raw)
					return
				}
				atomic.AddInt64(&writes, 1)
			}
		}(i)
	}

	wg.Wait()

	// Every serialized write must have applied exactly once on top of the
	// 6 seeded developers.
	want := int64(6) + atomic.LoadInt64(&writes)
	st, raw := do(t, c, http.MethodGet, ts.URL+"/stats", "")
	if st != http.StatusOK {
		t.Fatalf("final /stats: status=%d", st)
	}
	var sr statsResponse
	mustJSON(t, raw, &sr)
	if sr.Nodes[typeDeveloper] != want {
		t.Errorf("final Developer count = %d, want %d", sr.Nodes[typeDeveloper], want)
	}
}
