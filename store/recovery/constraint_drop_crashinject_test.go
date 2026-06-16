//go:build gograph_crashinject

package recovery

// Durability proof for DROP CONSTRAINT by-name (#1556). It drives the
// crashinject-helper to SIGKILL itself AFTER a durable DROP CONSTRAINT frame,
// so it compiles only under the gograph_crashinject build tag. Run with:
// go test -tags gograph_crashinject ./store/recovery/...

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/crashinject"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// TestConstraintDropCrash_PostWALSync is the both-absent durability proof for
// DROP CONSTRAINT by-name. The child commits a durable CREATE CONSTRAINT
// (UNIQUE) frame plus a node, commits a durable DROP CONSTRAINT frame, fsyncs,
// then SIGKILLs itself at constraint.drop.post-wal-sync — the drop is durable.
//
// Recovery over the resulting WAL must yield an EMPTY constraint set: the
// constraint and its UNIQUE backing index are removed together, because
// recovery reconstructs the backing index FROM the constraint set in a single
// frame — there is no torn intermediate where the constraint is gone but the
// index lingers (or vice-versa). The complementary both-present arm (the
// constraint surviving when no durable DROP follows it) is proven without crash
// injection by TestRecovery_ConstraintOpReplay /
// TestRecovery_ConstraintDropSuppressesCreate in the same package, which build
// the CREATE-only and CREATE+DROP WALs directly: together they show recovery
// lands on the constraint-present or constraint-absent state, never a partial.
func TestConstraintDropCrash_PostWALSync(t *testing.T) {
	const scenario = "constraint.drop.post-wal-sync"
	out, err := crashinject.Run(t, scenario, crashinject.Opts{})
	if err != nil {
		t.Fatalf("crashinject.Run(%s): %v", scenario, err)
	}
	if !out.Killed {
		t.Fatalf("child not SIGKILL'd at %s\nstdout: %s\nstderr: %s",
			scenario, out.Stdout, out.Stderr)
	}

	res, oerr := Open[string, float64](out.Dir, Options[string, float64]{
		Codec: txn.NewStringCodec(),
	})
	if oerr != nil {
		t.Fatalf("recovery.Open: %v", oerr)
	}
	if n := len(res.Constraints); n != 0 {
		t.Fatalf("recovered %d constraint(s) after a durable drop, want 0 (both constraint and backing index must be gone): %+v",
			n, res.Constraints)
	}
}
