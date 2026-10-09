//go:build gograph_crashinject

package recovery

import (
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/crashinject"
)

// TestLegacyMigrationCrash_RepeatsTheMigration kills the process (SIGKILL,
// through the crash-injection helper) at every point of the legacy migration
// checkpoint and requires the next recovery to finish the migration (rmp
// #3072): it must yield exactly the migrated graph and leave the store in the
// current format, whatever was on disk when the process died.
//
//   - replayed-pre-checkpoint: the legacy replay ran, nothing was written.
//   - snapshot.publish.archived-pre-rename: the legacy snapshot was moved to
//     snapshot.bak and the new one not yet renamed into place.
//   - checkpoint.control-tmp-pre-rename / checkpoint.control-renamed-pre-dirfsync:
//     the new snapshot is published and the WAL control file recording its
//     redo position is being replaced.
//   - published-pre-reclaim: the control file records the new redo position,
//     the legacy WAL segments are not yet reclaimed.
//
// A store with a single-file WAL (v015_undirected, written by v0.15.0) is also
// killed inside the conversion of that log to the segmented format the
// migration checkpoint performs: after the control file is written and before
// the legacy log is sealed (wal.migrate.control-written-pre-seal), and after
// the seal and before the first segmented frame
// (wal.migrate.sealed-pre-first-v2-frame).
func TestLegacyMigrationCrash_RepeatsTheMigration(t *testing.T) {
	points := []string{
		"recovery.legacy-migration.replayed-pre-checkpoint",
		"snapshot.publish.archived-pre-rename",
		"checkpoint.control-tmp-pre-rename",
		"checkpoint.control-renamed-pre-dirfsync",
		"recovery.legacy-migration.published-pre-reclaim",
	}
	fixtures := []struct {
		name  string
		check func(t *testing.T, g *lpg.Graph[string, int64])
		extra []string // crash points for this fixture only
	}{
		{legacyUndirectedFixture, func(t *testing.T, g *lpg.Graph[string, int64]) { assertLegacyMigrated(t, g) }, nil},
		// A v0.15.0 store: a version-3 manifest and a single-file WAL.
		{"v015_undirected", checkV015Undirected, []string{
			"wal.migrate.control-written-pre-seal",
			"wal.migrate.sealed-pre-first-v2-frame",
		}},
		{"simple_v4", func(t *testing.T, g *lpg.Graph[string, int64]) {
			if got := g.AdjList().Size(); got != 4 {
				t.Fatalf("Size = %d, want the 4 acknowledged relationships", got)
			}
			if g.HasEdgeHandle("a", "b", 301) {
				t.Fatal("the unacknowledged duplicate a->b (handle 301) was materialised")
			}
		}, nil},
	}
	for _, fx := range fixtures {
		for _, point := range append(slices.Clone(points), fx.extra...) {
			t.Run(fx.name+"/"+point, func(t *testing.T) {
				dir := copyFixture(t, fx.name)
				out, err := crashinject.Run(t, point, crashinject.Opts{
					Dir: dir,
					Env: []string{"GOGRAPH_CRASH_WORKLOAD=legacy-migration"},
				})
				if err != nil {
					t.Fatalf("crashinject.Run(%s): %v", point, err)
				}
				if !out.Killed {
					t.Fatalf("child not SIGKILL'd at %s (exit %d)\nstdout: %s\nstderr: %s",
						point, out.ExitCode, out.Stdout, out.Stderr)
				}
				res, err := Open[string, int64](dir, legacyOpts())
				if err != nil {
					t.Fatalf("recovery after a crash at %s: %v", point, err)
				}
				fx.check(t, res.Graph)
				requireMigratedOnDisk(t, dir)
				res2, err := Open[string, int64](dir, legacyOpts())
				if err != nil {
					t.Fatalf("second recovery: %v", err)
				}
				if res2.WALOps != 0 {
					t.Fatalf("second recovery replayed %d ops, want 0", res2.WALOps)
				}
				fx.check(t, res2.Graph)
			})
		}
	}
}
