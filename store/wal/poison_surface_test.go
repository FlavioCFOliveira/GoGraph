package wal_test

// poison_surface_test.go — rmp #2525.
//
// # What this gates
//
// The [wal.Writer] type documentation now states, per exported method, whether
// that method reports a POISONED writer. That table is a promise to callers —
// it is what lets a reader answer "which method tells me this handle is still
// usable" without running an experiment — so it needs a gate of its own, or the
// documentation drifts silently the first time a check order changes.
//
// The claim under test is the ASYMMETRY, not any single member: eight methods
// return the sticky error, two can return nil while poisoned, and one never
// consults it at all. A test that only asserted "appends are refused after a
// failed fsync" would keep passing straight through the change the
// documentation exists to warn about.
//
// # Why a switchable fsync rather than testfs.Faults
//
// [wal.Writer.Truncate] issues an fsync of its OWN, so what it returns on a
// poisoned writer depends on whether the fault that poisoned the writer is
// still firing. testfs.Faults{FailSyncAfter: N} is sticky once exhausted and
// can only express the persistent regime; the documentation describes both, so
// the fault here is a switch the test flips. Nothing else about the file is
// synthetic — it is a real os.File on a real path, which is what lets the
// "Truncate empties the file" assertion read the image with stat instead of
// trusting the writer's own bookkeeping.
//
// # Why identity and not errors.Is
//
// The Writer doc claims more than a class — "every subsequent Append/Sync
// returns the ORIGINAL error" — and [wal.Writer.Poisoned] claims to return "the
// same sentinel every subsequent Append/Sync returns". errors.Is would also
// accept a freshly wrapped error of the same class, which is the drift these
// assertions exist to catch. The class is checked separately, once, in the
// helper.
//
// This complements internal/sim's checkWALLifecycle (rmp #2472), which pins the
// same two surprises from the simulator side under a ONE-SHOT disk fault — so
// it observes only the transient regime — and does not reach AppendCtx, Sync,
// SyncCtx, TruncatePrefix or DurableOffset.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// poisonSurfacePayload is the frame payload size, so one frame costs
// wal.HeaderSize + poisonSurfacePayload bytes and every watermark below is
// exactly derivable rather than read back from the writer under test.
const poisonSurfacePayload = 16

// poisonSurfaceFrameBytes is the byte cost of one frame here.
const poisonSurfaceFrameBytes = wal.HeaderSize + poisonSurfacePayload

// errInjectedSync is the fsync failure this file injects. It is a distinct
// value so an assertion can tell "the writer handed back the sticky error"
// from "the writer handed back the raw device error" — the distinction the
// Truncate rows of the documentation turn on.
var errInjectedSync = errors.New("poison-surface: injected fsync failure")

// flakySyncFile is a real file whose Sync fails while the switch is set. Every
// other operation is the os.File's own, so the on-disk image is real.
type flakySyncFile struct {
	f    *os.File
	fail atomic.Bool
}

func (s *flakySyncFile) Write(p []byte) (int, error) { return s.f.Write(p) }
func (s *flakySyncFile) Read(p []byte) (int, error)  { return s.f.Read(p) }
func (s *flakySyncFile) Seek(off int64, whence int) (int64, error) {
	return s.f.Seek(off, whence)
}
func (s *flakySyncFile) Truncate(size int64) error { return s.f.Truncate(size) }
func (s *flakySyncFile) Close() error              { return s.f.Close() }
func (s *flakySyncFile) Sync() error {
	if s.fail.Load() {
		return errInjectedSync
	}
	return s.f.Sync()
}

// poisonedWriter is a freshly POISONED writer and everything an assertion needs
// to interrogate it.
type poisonedWriter struct {
	w    *wal.Writer
	file *flakySyncFile
	path string
	// durable is the watermark acknowledged BEFORE the failure; lost is the one
	// the poison discarded.
	durable, lost int64
	// sticky is the value [wal.Writer.Poisoned] reports.
	sticky error
}

// newPoisonedWriter drives one writer into the poison: commit 1 syncs cleanly,
// commit 2's fsync fails. When persistentFault is false the injected failure is
// switched off immediately afterwards, modelling a device that recovered; when
// it is true the failure keeps firing, so any later fsync fails too.
//
// The sequence is a straight line — no goroutines, no timing, no seeds.
func newPoisonedWriter(t *testing.T, persistentFault bool) poisonedWriter {
	t.Helper()
	path := filepath.Join(t.TempDir(), "poison_surface.wal")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // G304: path is this test's own t.TempDir() plus a literal leaf; there is no external input
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	file := &flakySyncFile{f: f}
	w, err := wal.OpenWith(file)
	if err != nil {
		t.Fatalf("wal.OpenWith: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	frame := make([]byte, poisonSurfacePayload)
	durable, err := w.AppendRun(func(emit func([]byte) error) error { return emit(frame) })
	if err != nil {
		t.Fatalf("first append: %v", err)
	}
	if err := w.SyncGroup(durable); err != nil {
		t.Fatalf("first sync must succeed before the fault is armed: %v", err)
	}
	lost, err := w.AppendRun(func(emit func([]byte) error) error { return emit(frame) })
	if err != nil {
		t.Fatalf("second append: %v", err)
	}
	file.fail.Store(true)
	if err := w.SyncGroup(lost); err == nil {
		t.Fatal("the second SyncGroup succeeded: the injected fsync fault did not fire, so nothing below would be a measurement of a poisoned writer")
	}
	if !persistentFault {
		file.fail.Store(false)
	}

	sticky := w.Poisoned()
	if sticky == nil {
		t.Fatal("Poisoned() reports healthy after a failed fsync: the writer was never poisoned, so every assertion below would be vacuous")
	}
	if !errors.Is(sticky, wal.ErrDurabilityFailed) {
		t.Fatalf("the sticky error %v does not carry wal.ErrDurabilityFailed", sticky)
	}
	if want := int64(poisonSurfaceFrameBytes); durable != want || lost != 2*want {
		t.Fatalf("watermarks are durable=%d lost=%d; one %d-byte frame each makes them %d and %d",
			durable, lost, poisonSurfaceFrameBytes, want, 2*want)
	}
	if got := imageSize(t, path); got != durable {
		t.Fatalf("the file holds %d byte(s) after the poison; the discarded suffix should leave exactly the %d durable byte(s)", got, durable)
	}
	return poisonedWriter{w: w, file: file, path: path, durable: durable, lost: lost, sticky: sticky}
}

// imageSize is the REAL on-disk length of the WAL, read with stat rather than
// taken from the writer's own bookkeeping: "Truncate empties the file" is a
// claim about the file, and asking the writer would let a bookkeeping-only
// reset pass as an emptied log.
func imageSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}

// TestWriter_PoisonedStateSurface is the gate for the per-method table in the
// [wal.Writer] type documentation: which exported methods report a poisoned
// writer, and which do not.
func TestWriter_PoisonedStateSurface(t *testing.T) {
	t.Parallel()

	// --- Group 1: these return the IDENTICAL sticky error, so a nil from any of
	// them does mean the writer was not poisoned at the call. ---
	t.Run("report_the_sticky_error", func(t *testing.T) {
		t.Parallel()
		frame := func() []byte { return make([]byte, poisonSurfacePayload) }
		cases := []struct {
			name string
			call func(p poisonedWriter) error
		}{
			{"Append", func(p poisonedWriter) error { return p.w.Append(frame()) }},
			{"AppendCtx", func(p poisonedWriter) error {
				return p.w.AppendCtx(context.Background(), frame())
			}},
			{"AppendRun", func(p poisonedWriter) error {
				_, err := p.w.AppendRun(func(emit func([]byte) error) error { return emit(frame()) })
				return err
			}},
			{"Sync", func(p poisonedWriter) error { return p.w.Sync() }},
			{"SyncCtx", func(p poisonedWriter) error { return p.w.SyncCtx(context.Background()) }},
			{"SyncGroup_discarded_watermark", func(p poisonedWriter) error { return p.w.SyncGroup(p.lost) }},
			{"SyncGroup_watermark_beyond_the_log", func(p poisonedWriter) error {
				return p.w.SyncGroup(p.lost + int64(poisonSurfaceFrameBytes))
			}},
			{"TruncatePrefix", func(p poisonedWriter) error {
				_, err := p.w.TruncatePrefix(p.durable)
				return err
			}},
			{"Close", func(p poisonedWriter) error { return p.w.Close() }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				// A writer of its own per case: the poison is terminal and several
				// of these mutate, so sharing one would make the result depend on
				// the order the cases happen to run in.
				p := newPoisonedWriter(t, false)
				err := tc.call(p)
				//nolint:errorlint // identity is the contract under test; see the file header
				if err != p.sticky {
					t.Errorf("on a poisoned writer this returned %v; the documented contract is the IDENTICAL sticky error %v that Poisoned() reports",
						err, p.sticky)
				}
			})
		}
	})

	// --- Group 2: these can return nil WHILE the writer is poisoned. ---
	t.Run("SyncBuffered_returns_nil_while_poisoned", func(t *testing.T) {
		t.Parallel()
		p := newPoisonedWriter(t, false)
		// Twice: the documentation says the nil is unconditional, not first-call.
		if err := p.w.SyncBuffered(); err != nil {
			t.Errorf("SyncBuffered on a poisoned writer returned %v; the documented behaviour is nil, because the poison rewinds the accepted offset "+
				"to the durable one and the already-durable fast path fires. If it now errors, its godoc and the Writer table must be re-judged", err)
		}
		if err := p.w.SyncBuffered(); err != nil {
			t.Errorf("the second SyncBuffered on a poisoned writer returned %v; the nil is documented as unconditional, not first-call-only", err)
		}
		// The load-bearing consequence: the nil did NOT mean health.
		if p.w.Poisoned() == nil {
			t.Error("the writer is no longer poisoned after SyncBuffered: a nil from it WOULD then mean health and the documented hazard has gone")
		}
		//nolint:errorlint // identity is the contract under test; see the file header
		if err := p.w.Append(make([]byte, poisonSurfacePayload)); err != p.sticky {
			t.Errorf("after a nil SyncBuffered an append returned %v, not the sticky %v: the nil must not have made the writer usable", err, p.sticky)
		}
	})

	t.Run("SyncGroup_already_durable_watermark_returns_nil_while_poisoned", func(t *testing.T) {
		t.Parallel()
		p := newPoisonedWriter(t, false)
		// rmp #2322: durability is tested before the poison, so a committer whose
		// marker is already on the platter is not told its commit failed.
		if err := p.w.SyncGroup(p.durable); err != nil {
			t.Errorf("SyncGroup for the ALREADY-DURABLE watermark %d returned %v on a poisoned writer; rmp #2322 fixed exactly this — "+
				"a committer whose marker is durable must not be told its commit failed", p.durable, err)
		}
		if p.w.Poisoned() == nil {
			t.Error("the writer is no longer poisoned after the already-durable SyncGroup: the nil would then mean health")
		}
	})

	// --- Group 3: Truncate never consults the sticky error. Both fault regimes,
	// because what it RETURNS differs between them while everything that
	// matters for durability does not. ---
	for _, regime := range []struct {
		name       string
		persistent bool
	}{
		{"transient_fault_returns_nil", false},
		{"persistent_fault_returns_the_raw_fsync_error", true},
	} {
		t.Run("Truncate_on_poisoned_"+regime.name, func(t *testing.T) {
			t.Parallel()
			p := newPoisonedWriter(t, regime.persistent)
			statsBefore := p.w.Stats()
			sizeBefore := imageSize(t, p.path)
			freed, err := p.w.Truncate()
			sizeAfter := imageSize(t, p.path)

			// What differs between the regimes: only the returned error.
			if regime.persistent {
				if !errors.Is(err, errInjectedSync) {
					t.Errorf("under a PERSISTENT fsync fault Truncate returned %v; documented behaviour is the raw error from its own fsync", err)
				}
				//nolint:errorlint // identity is the contract under test; see the file header
				if err == p.sticky {
					t.Error("Truncate returned the identical STICKY error: the documentation says it does not consult the poison and reports its own fsync failure instead")
				}
				if errors.Is(err, wal.ErrDurabilityFailed) {
					t.Errorf("Truncate's error %v carries wal.ErrDurabilityFailed; the documentation warns callers that errors.Is against that class is FALSE here, "+
						"so a caller cannot use it to detect the poison", err)
				}
			} else if err != nil {
				t.Errorf("under a TRANSIENT fsync fault Truncate returned %v; documented behaviour is nil — a success on a writer that is still dead", err)
			}

			// What holds in BOTH regimes, and is what the safety argument rests on.
			if freed != p.durable {
				t.Errorf("Truncate reported freeing %d byte(s); the poisoned writer's file held the %d durable byte(s) — the un-synced suffix was already discarded",
					freed, p.durable)
			}
			if sizeBefore != p.durable || sizeAfter != 0 {
				t.Errorf("the file went from %d byte(s) to %d across Truncate; documented behaviour is the %d durable byte(s) emptied to 0",
					sizeBefore, sizeAfter, p.durable)
			}
			if off := p.w.DurableOffset(); off != 0 {
				t.Errorf("the durable offset after Truncate is %d; the file was emptied, so it must be 0 or a later rollback truncates to a stale size", off)
			}
			if statsBefore != p.w.Stats() {
				t.Errorf("Truncate changed the LIFETIME counters, %+v -> %+v; they are documented as not reset", statsBefore, p.w.Stats())
			}
			//nolint:errorlint // identity is the contract under test; see the file header
			if got := p.w.Poisoned(); got != p.sticky {
				t.Errorf("Poisoned() returns %v after Truncate, not the identical sticky %v: emptying the file cleared or replaced the fail-stop, "+
					"so appends onto a WAL whose durability failed could resume", got, p.sticky)
			}
			//nolint:errorlint // identity is the contract under test; see the file header
			if err := p.w.Append(make([]byte, poisonSurfacePayload)); err != p.sticky {
				t.Errorf("an append after Truncate returned %v, not the sticky %v: the emptied file is accepting frames, which is the durability hole "+
					"Truncate-on-poisoned is only safe in the absence of", err, p.sticky)
			}
		})
	}

	// --- Stats and DurableOffset have no error channel; SyncFailed is what
	// surfaces the failure there. ---
	t.Run("Stats_and_DurableOffset_surface_the_failure_without_an_error", func(t *testing.T) {
		t.Parallel()
		p := newPoisonedWriter(t, false)
		st := p.w.Stats()
		if st.SyncFailed != 1 {
			t.Errorf("Stats.SyncFailed is %d after exactly one failed round; expected 1", st.SyncFailed)
		}
		if off := p.w.DurableOffset(); off != p.durable {
			t.Errorf("DurableOffset is %d after the poison; it must still be the %d byte(s) acknowledged BEFORE the failure — the un-synced suffix was discarded",
				off, p.durable)
		}
		if st.Bytes != uint64(p.lost) {
			t.Errorf("Stats.Bytes is %d; it counts every frame ACCEPTED including the one the poison discarded, which is %d byte(s)", st.Bytes, p.lost)
		}
	})

	// --- After Close the closed sentinel replaces the sticky error everywhere
	// except Poisoned. ---
	t.Run("after_Close_the_closed_sentinel_replaces_the_poison", func(t *testing.T) {
		t.Parallel()
		p := newPoisonedWriter(t, false)
		if cerr := p.w.Close(); cerr == nil {
			t.Fatal("Close on a poisoned writer returned nil; it is documented to return the sticky error")
		}
		frame := make([]byte, poisonSurfacePayload)
		_, arErr := p.w.AppendRun(func(emit func([]byte) error) error { return emit(frame) })
		_, tErr := p.w.Truncate()
		_, tpErr := p.w.TruncatePrefix(p.durable)
		closedCases := []struct {
			name string
			err  error
		}{
			{"Append", p.w.Append(frame)},
			{"AppendCtx", p.w.AppendCtx(context.Background(), frame)},
			{"AppendRun", arErr},
			{"Sync", p.w.Sync()},
			{"SyncCtx", p.w.SyncCtx(context.Background())},
			{"SyncGroup", p.w.SyncGroup(p.lost)},
			{"SyncBuffered", p.w.SyncBuffered()},
			// Truncate is in this list precisely because it is the method that
			// ignores the poison: only the CLOSED check stops it.
			{"Truncate", tErr},
			{"TruncatePrefix", tpErr},
			{"Close", p.w.Close()},
		}
		for _, c := range closedCases {
			if !errors.Is(c.err, wal.ErrWriterClosed) {
				t.Errorf("after Close, %s returned %v; the closed check is documented to precede the poison check, so it must report wal.ErrWriterClosed",
					c.name, c.err)
			}
		}
		//nolint:errorlint // identity is the contract under test; see the file header
		if got := p.w.Poisoned(); got != p.sticky {
			t.Errorf("Poisoned() on a CLOSED poisoned writer returned %v, not the identical sticky %v: the owner can no longer tell why the handle died",
				got, p.sticky)
		}
	})
}

// TestWriter_TruncatePrefix_PoisonPrecedesUnsupported pins the check ORDER the
// [wal.Writer.TruncatePrefix] documentation now states: the sticky-poison check
// runs before the path-less rejection and before the range and upTo == 0 checks.
//
// The discriminator is that the SAME path-less writer returns two different
// errors depending only on its health, so the test cannot pass on a writer that
// was never poisoned, nor on one whose path check happens to fire first.
func TestWriter_TruncatePrefix_PoisonPrecedesUnsupported(t *testing.T) {
	t.Parallel()
	// A path-less writer (OpenWith): TruncatePrefix is unsupported on it.
	p := newPoisonedWriter(t, false)

	// The HEALTHY leg has to be taken on a separate writer, since the poison is
	// terminal — without it the poisoned leg below proves nothing about ordering.
	healthyPath := filepath.Join(t.TempDir(), "healthy_pathless.wal")
	hf, err := os.OpenFile(healthyPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // G304: as above — this test's own t.TempDir() plus a literal leaf
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	hw, err := wal.OpenWith(&flakySyncFile{f: hf})
	if err != nil {
		t.Fatalf("wal.OpenWith: %v", err)
	}
	t.Cleanup(func() { _ = hw.Close() })
	mark, err := hw.AppendRun(func(emit func([]byte) error) error {
		return emit(make([]byte, poisonSurfacePayload))
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := hw.SyncGroup(mark); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, err := hw.TruncatePrefix(mark); !errors.Is(err, wal.ErrPrefixTruncateUnsupported) {
		t.Fatalf("on a HEALTHY path-less writer TruncatePrefix returned %v; expected wal.ErrPrefixTruncateUnsupported", err)
	}

	// POISONED and path-less: the poison check decides, because it runs first.
	got, err := p.w.TruncatePrefix(p.durable)
	//nolint:errorlint // identity is the contract under test; see the file header
	if err != p.sticky {
		t.Errorf("on a POISONED path-less writer TruncatePrefix returned %v; the documented order puts the sticky-poison check first, so it must return "+
			"the identical sticky error %v rather than wal.ErrPrefixTruncateUnsupported", err, p.sticky)
	}
	if got != 0 {
		t.Errorf("TruncatePrefix reported reclaiming %d byte(s) while returning an error; it must touch nothing and report 0", got)
	}
	// upTo == 0 is documented as a no-op on a usable writer; the poison check
	// pre-empts that too.
	//nolint:errorlint // identity is the contract under test; see the file header
	if _, err := p.w.TruncatePrefix(0); err != p.sticky {
		t.Errorf("TruncatePrefix(0) on a poisoned writer returned %v; the documented order puts the sticky-poison check before the upTo == 0 no-op, "+
			"so it must return the identical sticky error %v", err, p.sticky)
	}
	// An out-of-range upTo likewise never reaches its own rejection.
	//nolint:errorlint // identity is the contract under test; see the file header
	if _, err := p.w.TruncatePrefix(p.lost * 1000); err != p.sticky {
		t.Errorf("TruncatePrefix with an out-of-range upTo returned %v on a poisoned writer; the sticky-poison check precedes the range check, "+
			"so it must return the identical sticky error %v", err, p.sticky)
	}
}
