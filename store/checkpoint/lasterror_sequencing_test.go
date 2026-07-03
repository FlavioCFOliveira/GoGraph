package checkpoint

// lasterror_sequencing_test.go — regression coverage for rmp #1873:
// "Checkpointer.RunCheckpoint doc overstates concurrency safety; LastError
// can be masked".
//
// Background. RunCheckpoint's own doc claimed it was "safe to interleave
// with Trigger/loop runs because every run serialises on the same commit
// lock" — false: phase 2 (the dominant-duration snapshot write) is
// deliberately lock-free, so two concurrent checkpoint attempts can both be
// writing to the same snapshot directory at once and collide with a real
// filesystem error. The doc has been corrected to explicitly forbid this
// usage rather than retrofit real mutual exclusion onto a phase that is
// lock-free specifically so writers are never blocked by a (potentially
// multi-second) snapshot write.
//
// A second, independent defect survives even after that doc fix, because a
// caller CAN still violate the documented contract: setErr used to
// unconditionally overwrite Stats().LastError on every call, so if two
// concurrent attempts interleave such that an OLDER (earlier-started)
// attempt's outcome is recorded AFTER a NEWER (later-started) attempt's own
// outcome, the older attempt's belated setErr call would silently mask the
// newer one's result — including masking a real failure with a success that
// is, from the caller's perspective, actually stale.
//
// Fixed by minting one monotonic sequence number per attempt (at the very
// start of runNonBlocking) and having setErr reject a write whose sequence
// number is lower than the one already recorded: whichever attempt started
// LAST always wins, regardless of which one happens to COMPLETE last.

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// newLastErrorCheckpointer builds a minimal, unstarted Checkpointer
// suitable for driving setErr directly — no background loop, no real
// checkpoint I/O — mirroring newRaceCheckpointer's construction
// (trigger_stop_race_test.go) without calling Start.
func newLastErrorCheckpointer(t *testing.T) *Checkpointer[string, int64] {
	t.Helper()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	var mu sync.Mutex
	return New(Config{Dir: dir, MaxAge: 0}, g, w, &mu)
}

// TestSetErr_StaleOlderAttemptCannotMaskNewerOutcome is the load-bearing
// proof: an attempt sequenced BEFORE another (lower seq) that happens to
// call setErr AFTER it (simulating the out-of-completion-order race two
// concurrent RunCheckpoint calls could produce) must not overwrite the
// later-sequenced attempt's already-recorded result — in either direction
// (a stale success must not erase a newer failure, and a stale failure must
// not erase a newer success).
func TestSetErr_StaleOlderAttemptCannotMaskNewerOutcome(t *testing.T) {
	t.Parallel()

	t.Run("stale success does not mask a newer failure", func(t *testing.T) {
		t.Parallel()
		c := newLastErrorCheckpointer(t)
		newerErr := errors.New("newer attempt: disk full")

		c.setErr(2, newerErr) // attempt #2 (started later) fails first
		c.setErr(1, nil)      // attempt #1 (started earlier) succeeds, but arrives late

		if got := c.Stats().LastError; got != newerErr.Error() {
			t.Fatalf("LastError = %q, want %q (the newer failure must survive the stale success)", got, newerErr.Error())
		}
	})

	t.Run("stale failure does not mask a newer success", func(t *testing.T) {
		t.Parallel()
		c := newLastErrorCheckpointer(t)
		staleErr := errors.New("older attempt: transient error")

		c.setErr(2, nil)      // attempt #2 (started later) succeeds first
		c.setErr(1, staleErr) // attempt #1 (started earlier) fails, but arrives late
		if got := c.Stats().LastError; got != "" {
			t.Fatalf("LastError = %q, want \"\" (the newer success must survive the stale failure)", got)
		}
	})
}

// TestSetErr_SequentialUsageUnaffected guards against the fix regressing the
// documented, supported usage: a plain series of sequential (non-concurrent)
// attempts, where each new attempt's sequence number is strictly greater
// than the last, must behave exactly as an unconditional overwrite would —
// every call's outcome is visible until superseded by the next.
func TestSetErr_SequentialUsageUnaffected(t *testing.T) {
	t.Parallel()
	c := newLastErrorCheckpointer(t)
	err1 := errors.New("first failure")

	c.setErr(1, err1)
	if got := c.Stats().LastError; got != err1.Error() {
		t.Fatalf("after attempt 1: LastError = %q, want %q", got, err1.Error())
	}

	c.setErr(2, nil)
	if got := c.Stats().LastError; got != "" {
		t.Fatalf("after attempt 2 (success): LastError = %q, want \"\"", got)
	}

	err3 := errors.New("third failure")
	c.setErr(3, err3)
	if got := c.Stats().LastError; got != err3.Error() {
		t.Fatalf("after attempt 3: LastError = %q, want %q", got, err3.Error())
	}
}

// TestSetErr_EqualSeqOverwrites confirms the gate is "seq >= lastErrSeq", not
// strictly greater: a second call recording the SAME attempt's final outcome
// (a scenario that cannot arise from runNonBlocking's own single terminal
// setErr call per attempt, but is a reasonable direct-unit-test boundary to
// pin) still updates, rather than being silently dropped as "stale".
func TestSetErr_EqualSeqOverwrites(t *testing.T) {
	t.Parallel()
	c := newLastErrorCheckpointer(t)
	first := errors.New("first")
	second := errors.New("second, same seq")

	c.setErr(5, first)
	c.setErr(5, second)

	if got := c.Stats().LastError; got != second.Error() {
		t.Fatalf("LastError = %q, want %q (equal seq must still overwrite)", got, second.Error())
	}
}

// TestRunCheckpoint_SequentialCallsReportCorrectLastError is an end-to-end
// sanity check (not a race test) that real, sequential RunCheckpoint calls —
// the documented, supported usage — still correctly track LastError through
// the sequence-numbering machinery: this is not a special case runNonBlocking
// needs to handle differently, just the ordinary path with seq always
// strictly increasing.
func TestRunCheckpoint_SequentialCallsReportCorrectLastError(t *testing.T) {
	t.Parallel()
	c := newLastErrorCheckpointer(t)

	if err := c.RunCheckpoint(); err != nil {
		t.Fatalf("first RunCheckpoint: %v", err)
	}
	if got := c.Stats().LastError; got != "" {
		t.Fatalf("after a successful RunCheckpoint: LastError = %q, want \"\"", got)
	}

	if err := c.RunCheckpoint(); err != nil {
		t.Fatalf("second RunCheckpoint: %v", err)
	}
	if got := c.Stats().LastError; got != "" {
		t.Fatalf("after a second successful RunCheckpoint: LastError = %q, want \"\"", got)
	}
}
