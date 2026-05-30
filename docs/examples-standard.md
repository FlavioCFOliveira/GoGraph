# Examples standard

This document defines the single, reusable standard that every example
under `examples/01_*` through `examples/23_*` must follow. It exists so
that each example is **runnable**, **self-documenting**, and — above
all — **regression-tested**: the output an example prints is not just a
demonstration, it is a baseline that a test pins down so a future change
cannot silently break it.

The two multi-file references `examples/24_social_network_cli` and
`examples/25_software_house_api` already meet this bar; treat them as
the worked end state, and this document as the recipe for bringing the
single-file examples up to it.

The standard has three pillars:

1. **Testable extraction** — move the logic out of `func main()` so a
   test can drive it and capture its output.
2. **A regression test** — a Go testable example for deterministic
   output, or an assertion-based test for output that is not
   byte-stable.
3. **A per-example `README.md`** — written from one uniform template.

---

## Pillar 1 — Testable extraction

Every example refactors its logic out of `func main()` into a single
function:

```go
func run(w io.Writer) error
```

`run` does all the work and writes every byte of its output to `w`.
`main()` becomes a thin wrapper that wires `run` to the real process:
it passes `os.Stdout` and converts a returned error into a fatal exit.

The rule is absolute: **inside `run`, nothing writes to `os.Stdout` or
to `os.Stderr` directly, and nothing calls `fmt.Println` /
`fmt.Printf` without a writer.** Use `fmt.Fprintf(w, …)` and
`fmt.Fprintln(w, …)` exclusively. This is what makes the output
capturable by a `bytes.Buffer` in a test and comparable against a
`// Output:` block in a testable example.

### Before

```go
func main() {
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	// … build the graph …
	d, err := search.Dijkstra(c, src)
	if err != nil {
		log.Fatalf("Dijkstra: %v", err)
	}
	fmt.Printf("Lisbon -> %-7s : %4d km\n", "Madrid", dist) // writes to os.Stdout
}
```

### After

```go
func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run builds the graph, runs the query, and writes the report to w.
// All output goes to w so a test can capture and assert it.
func run(w io.Writer) error {
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	// … build the graph …
	d, err := search.Dijkstra(c, src)
	if err != nil {
		return fmt.Errorf("Dijkstra: %w", err)
	}
	fmt.Fprintf(w, "Lisbon -> %-7s : %4d km\n", "Madrid", dist) // writes to w
	return nil
}
```

Notes:

- `run` **returns** errors (`return fmt.Errorf("…: %w", err)`); it never
  calls `log.Fatal` itself. Only `main()` is allowed to terminate the
  process. This keeps `run` testable: a test can assert on the returned
  error instead of having the process exit.
- The example's leading package doc comment stays as-is; it already
  claims the output "serves as the regression baseline". Pillar 2 is
  what makes that claim true.

---

## Pillar 2 — Regression test standard

Each example carries exactly one regression test that pins its output.
Which form you use depends on whether the example's stdout is
byte-stable.

### Default — deterministic stdout → Go testable example

When the output is byte-for-byte stable across runs and machines, add
an `example_test.go` in `package main` containing a Go **testable
example**:

```go
package main

import "os"

func Example() {
	_ = run(os.Stdout)
	// Output:
	// Lisbon -> Madrid  :  624 km
	// Lisbon -> Paris   : 1737 km
	// Lisbon -> Rome    : 2046 km
}
```

Go's test framework captures everything the example writes to
`os.Stdout` and compares it against the text in the `// Output:` block.
If they differ, the test fails. This runs under plain `go test ./...`
with no build tags. **This is the default for the majority of the
examples.**

The `_ =` on `run(os.Stdout)` is deliberate: a testable example cannot
return an error, so the error is discarded here. If the example can
realistically fail in a way worth asserting on, prefer the
assertion-based form below instead.

### Non-deterministic stdout → assertion-based `Test*`

Some examples produce output that is **not** byte-stable: it varies with
timing, the network, the filesystem, or goroutine scheduling. A
`// Output:` block would be flaky for these. Instead, write a normal
test function that drives `run` into a `bytes.Buffer` and asserts only
the **deterministic invariants** — counts, the presence of expected
lines, recovered data — never the volatile parts (durations, absolute
paths, ordering that depends on scheduling).

```go
package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()

	// Assert deterministic invariants, not volatile output.
	if !strings.Contains(out, "Converged") {
		t.Errorf("expected a convergence line, got:\n%s", out)
	}
	if got := strings.Count(out, "node "); got != 5 {
		t.Errorf("expected 5 node lines, got %d:\n%s", got, out)
	}
}
```

This mirrors `bolt/server/example_test.go`, the in-repo precedent: it
drives a non-deterministic network round-trip and asserts the
deterministic query result (`n = 1`) rather than any wire-level or
timing-dependent output.

#### Examples that are non-deterministic, and why

| Example | Why its stdout is not byte-stable |
|---|---|
| `examples/10_dimacs9_routing` | Reports latency percentiles; timings are environment-dependent. |
| `examples/17_transactional_log` | Background checkpoint timing and the stats it prints vary per run. |
| `examples/23_bolt_server` | Drives a network round-trip; timing is non-deterministic. |
| `examples/20_concurrent_reads` | Spawns goroutines; completion order is non-deterministic. Assert the aggregate results, not per-goroutine ordering. |

These four examples use the assertion-based `Test*` form. Every other
example in the `01`–`23` range uses the default testable-example form
unless the temp-dir caveat below applies.

#### Temp-directory caveat (04, 05, 17, 18, 21)

The following examples write to a directory created with `os.MkdirTemp`,
whose absolute path is different on every run:

`examples/04_persistence`, `examples/05_out_of_core`,
`examples/17_transactional_log`, `examples/18_oocore_pipeline`,
`examples/21_typed_recovery`.

The temp path must **never** appear in a `// Output:` block — it would
make the testable example flaky. For these, either:

- print only deterministic content (keep the temp path out of stdout
  entirely, so the testable-example form still works); or
- use the assertion-based `Test*` form and assert on the stable parts
  of the output.

`examples/17_transactional_log` is in both lists: it is non-deterministic
(checkpoint timing) **and** writes to a temp directory, so it uses the
assertion-based form.

### goleak — examples that spawn goroutines or a server

Examples that start goroutines or a server must double as a
goroutine-leak check. `go.uber.org/goleak` is already a direct
dependency of the module (see `go.mod`). Add a `TestMain` to the
example's test package that wraps the suite:

```go
package main

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs every test in this package under go.uber.org/goleak so
// the example doubles as a goroutine-leak check.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
```

This is the same pattern `examples/24_social_network_cli/cli_test.go`
uses. The examples that need it are:

| Example | Why it needs a goleak guard |
|---|---|
| `examples/17_transactional_log` | Runs a background checkpoint goroutine. |
| `examples/20_concurrent_reads` | Spawns worker goroutines for the concurrent reads. |
| `examples/23_bolt_server` | Starts a server with per-connection goroutines. |

Run the tests for these three examples under the race detector:

```sh
go test -race ./examples/17_transactional_log/...
go test -race ./examples/20_concurrent_reads/...
go test -race ./examples/23_bolt_server/...
```

---

## Pillar 3 — Per-example README

Every example gets a `README.md` written from the uniform template
below. Keep it **concise** for trivial examples (a one-line "What it
demonstrates", a short expected-output block) and **fuller** for complex
ones, but always keep every section heading present and in the same
order so the set reads consistently.

### README template (copy-paste ready)

```markdown
# Example NN — <Title>

## What it demonstrates

<One or two sentences. What GoGraph capability does this example show?>

## Domain / scenario

<The concrete scenario the example models — the toy graph, the dataset,
the workload. One short paragraph.>

## How to run

\`\`\`sh
go run ./examples/<NN_name>
\`\`\`

## Expected output

\`\`\`
<The exact deterministic lines this example prints. For
non-deterministic examples, show a representative run and note which
parts vary (timings, paths).>
\`\`\`

## Key APIs

- `pkg/path.Symbol` — <what it is used for here>
- `pkg/path.Symbol` — <what it is used for here>

## Further reading

- [`pkg/path`](../../pkg/path) — package documentation
- [Example MM — <Title>](../MM_name) — related example
- [docs/<topic>.md](../../docs/<topic>.md) — relevant design note
```

### Section meanings

- **What it demonstrates** — the single capability or technique. One or
  two sentences.
- **Domain / scenario** — the concrete toy domain (cities and roads, a
  social graph, a build-dependency DAG …) so the reader knows what the
  numbers mean.
- **How to run** — always the `go run ./examples/<NN_name>` invocation,
  plus any flags the example accepts.
- **Expected output** — a fenced block holding the exact deterministic
  output. This block and the test's `// Output:` block (or asserted
  lines) must agree. For a non-deterministic example, show a
  representative run and call out which parts vary.
- **Key APIs** — a short list of the GoGraph packages and functions the
  example exercises, each with a one-line note. This is the index a
  reader scans to find "which example shows package X".
- **Further reading** — links to the relevant package docs, to related
  examples, and to any design note under `docs/`. Use relative links
  from the example directory (e.g. `../../search` for the `search`
  package, `../08_pagerank` for a sibling example).

---

## How to bring an example to the standard

A checklist for each example, in order:

1. **Refactor.** Extract the body of `main()` into
   `func run(w io.Writer) error`. Replace every `fmt.Print*` /
   `os.Stdout` write inside the logic with `fmt.Fprint*(w, …)`. Make
   `main()` a thin `run(os.Stdout)` + `log.Fatal` wrapper. Return errors
   from `run` instead of calling `log.Fatal` inside it.
2. **README.** Add `README.md` from the template above, filling every
   section.
3. **Test.** Add the regression test:
   - deterministic stdout → `example_test.go` with `func Example()` and
     a `// Output:` block;
   - non-deterministic stdout (10, 17, 20, 23) → `func TestRun` driving
     `run` into a `bytes.Buffer` and asserting the stable invariants;
   - temp-dir examples (04, 05, 17, 18, 21) → keep the temp path out of
     any `// Output:` block;
   - goroutine/server examples (17, 20, 23) → add a `TestMain` wrapping
     `goleak.VerifyTestMain(m)` and run under `-race`.
4. **Verify green.** Run, in order:

   ```sh
   gofmt -l ./examples/<NN_name>      # must print nothing
   go vet ./examples/<NN_name>/...
   go test ./examples/<NN_name>/...   # add -race for 17, 20, 23
   ```

   The expected-output block in the README, the example's leading doc
   comment, and the test's `// Output:` (or asserted lines) must all
   describe the same output.

---

## References

- `examples/24_social_network_cli` — a multi-file CLI example that
  already meets this standard: logic factored out of `main`, golden-file
  regression tests, and a `TestMain` goleak guard
  (`examples/24_social_network_cli/cli_test.go`).
- `examples/25_software_house_api` — a multi-file persistent REST
  example that already meets this standard: a full test matrix
  (seed, endpoints, concurrency under `-race`, in-process reopen, and a
  real `kill -9` cross-process restart) with a `go.uber.org/goleak`
  guard.
- `bolt/server/example_test.go` — the in-repo precedent for an
  assertion-based test that drives a non-deterministic round-trip with
  clean teardown and a no-leak guarantee. Mirror it for the
  non-deterministic examples.
