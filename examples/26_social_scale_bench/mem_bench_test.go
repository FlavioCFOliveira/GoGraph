package main

import (
	"context"
	"io"
	"os"
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// benchScale reads the build scale from env so the profiling scale can be
// varied without editing code: MEMBENCH_USERS / MEMBENCH_ARTICLES.
func benchScale() config {
	c := config{users: 40000, articles: 4000, friendsMin: 150, friendsMax: 200, likesMax: 300, seed: 1, relTypes: true}
	if v := os.Getenv("MEMBENCH_USERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.users = n
		}
	}
	if v := os.Getenv("MEMBENCH_ARTICLES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.articles = n
		}
	}
	if os.Getenv("MEMBENCH_RELTYPES") == "0" {
		c.relTypes = false
	}
	return c
}

// BenchmarkBuild builds the graph once (use -benchtime=1x) so a heap
// profile captured with -memprofile reflects the live graph that build
// produces. The returned graph is kept alive past the timed region via
// keepAlive so the in-use heap profile attributes the resident memory.
func BenchmarkBuild(b *testing.B) {
	cfg := benchScale()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		// Weightless mirrors run() in main.go: the social graph is queried only
		// by relationship type / edge properties, so the per-edge weight column
		// is dead memory and dropped. The heap profile thus reflects the real
		// resident shape the example runs against.
		g := lpg.New[string, float64](adjlist.Config{Directed: true, Weightless: true})
		if _, err := build(context.Background(), g, cfg, io.Discard); err != nil {
			b.Fatal(err)
		}
		keepAlive = g
	}
}

// keepAlive pins the most recent graph so it stays reachable when the
// heap profile is written at the end of the benchmark.
var keepAlive *lpg.Graph[string, float64]
