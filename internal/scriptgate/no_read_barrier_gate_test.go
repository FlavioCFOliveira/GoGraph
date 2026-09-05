package scriptgate

// no_read_barrier_gate_test.go — rmp #2344 AC4: pin, as a test rather than as
// prose, that NO production read path acquires a barrier.
//
// # Why this is a gate and not a comment
//
// The module spent several sprints retiring pre-MVCC exclusion, and the last of it
// — lpg.Graph.View — was removed in rmp #2344. Every one of those retirements was
// justified by "reads take no barrier", and that justification is load-bearing for
// the read-scaling claims and for the argument that a DDL is the only thing an
// ordinary write excludes.
//
// Nothing enforced it. A single reintroduced acquisition on a read path would
// restore the reader-starvation shape rmp #2274 measured at -51x, and it would do so
// silently: the suite would stay green, because a barrier does not break
// correctness, only scaling. This gate makes the regression a build failure.
//
// It is a SOURCE gate deliberately. A behavioural test would have to provoke the
// starvation to detect it, which is exactly the kind of timing-dependent oracle that
// passes on a quiet machine; the property being defended is structural, so the
// structure is what gets checked.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoReadBarrierGate asserts that lpg.Graph.View no longer exists anywhere in the
// module, and that no production file outside graph/lpg reaches for the visibility
// gate directly.
//
// graph/lpg itself is exempt from the second check: it OWNS the gate, and the DDL
// path (Graph.ApplyAtomically) legitimately takes it strongly. What must not happen
// is a caller elsewhere acquiring it, or Graph.View coming back.
func TestNoReadBarrierGate(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)

	var viewSites, gateSites []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Skip VCS, build output and the reference clones a spike may have left.
			//
			// ".claude" carries agent tooling state, and in particular NESTED GIT
			// WORKTREES (.claude/worktrees/*) — a second checkout of this very
			// module. Without this the walk reads that checkout's own graph/lpg
			// sources as if they were external callers and reports every
			// legitimate in-lpg acquisition as a violation: 14 false sites,
			// enough to turn `make ci` red for the duration of an isolated
			// agent run. A nested checkout is not this module's source.
			switch info.Name() {
			case ".git", "vendor", "testdata", ".claude":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		// This gate's own source names the forbidden strings; exempt it, or it
		// fails on itself.
		if strings.HasSuffix(rel, "no_read_barrier_gate_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path) //nolint:gosec // walking the module's own tree
		if rerr != nil {
			return rerr
		}
		for i, line := range strings.Split(string(b), "\n") {
			code := line
			if idx := strings.Index(code, "//"); idx >= 0 {
				code = code[:idx] // comments may discuss the retired method freely
			}
			if strings.Contains(code, ".View(func()") || strings.Contains(code, "func (g *Graph[N, W]) View(") {
				viewSites = append(viewSites, formatSite(rel, i+1, line))
			}
			if strings.HasPrefix(rel, "graph/lpg/") {
				continue
			}
			if strings.Contains(code, "visGate.") || strings.Contains(code, "visMu.") {
				gateSites = append(gateSites, formatSite(rel, i+1, line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}

	if len(viewSites) != 0 {
		t.Errorf("lpg.Graph.View is back, at %d site(s):\n%s\n\n"+
			"It was removed in rmp #2344 because it provided NO isolation: since rmp #2320 an "+
			"ordinary write holds the visibility barrier SHARED, so a View reader using "+
			"unversioned accessors reads another transaction's uncommitted work (7040 "+
			"partial-transaction observations against ZERO from a snapshot reader over "+
			"6 488 034 reads). A caller that needs a consistent view of DATA takes a SNAPSHOT "+
			"(Graph.BeginRead + Graph.ReadAt + Graph.EndRead); a caller that needs the CATALOG "+
			"is covered by the engine's schema gate",
			len(viewSites), strings.Join(viewSites, "\n"))
	}
	if len(gateSites) != 0 {
		t.Errorf("the visibility gate is acquired from OUTSIDE graph/lpg, at %d site(s):\n%s\n\n"+
			"graph/lpg owns that gate and the DDL path takes it strongly; nothing else may "+
			"acquire it. A read path that does restores the reader-starvation shape rmp #2274 "+
			"measured at -51x, and does it silently, because a barrier costs scaling and not "+
			"correctness",
			len(gateSites), strings.Join(gateSites, "\n"))
	}
}

// formatSite renders one offending line for the failure message.
func formatSite(rel string, line int, text string) string {
	return "  " + rel + ":" + itoa(line) + ": " + strings.TrimSpace(text)
}

// itoa avoids pulling strconv in for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// moduleRoot walks up from the working directory to the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, serr := os.Stat(filepath.Join(dir, "go.mod")); serr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from the working directory")
		}
		dir = parent
	}
}
