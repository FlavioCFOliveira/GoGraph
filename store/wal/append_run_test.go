package wal

// append_run_test.go — contiguity of a transaction's frames under concurrent
// appenders (rmp #2302, audit finding E5).
//
// Design: docs/design-wal-transaction-contiguity.md.
//
// The claim is not "AppendRun writes the frames". It is "no other appender's
// frame can land between them", and the only way to establish that is to have
// another appender trying, hard, at the same time.
//
// Every test here runs BOTH arms — the run and the loop of individual appends —
// against the same workload, because the failure mode is silent: interleaved
// frames make crash recovery discard COMMITTED ops and increment a counter.
// Deleting the losing arm once the fix is in would leave the next person to touch
// the append path with nothing to see.

import (
	"bytes"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// txnFrames builds the payloads of one transaction: n op frames and a trailing
// marker, every one of them carrying the transaction's own tag so a reader can
// tell whose frame it is.
func txnFrames(tag string, n int) [][]byte {
	out := make([][]byte, 0, n+1)
	for i := 0; i < n; i++ {
		out = append(out, []byte(fmt.Sprintf("%s:op%d", tag, i)))
	}
	out = append(out, []byte(tag+":commit"))
	return out
}

// tagOf returns the transaction tag a frame belongs to, or "" for a frame this
// test did not write.
func tagOf(payload []byte) string {
	if i := bytes.IndexByte(payload, ':'); i > 0 {
		return string(payload[:i])
	}
	return ""
}

// readTags reads the WAL at path and returns the tag of every frame, in file
// order.
func readTags(t *testing.T, path string) []string {
	t.Helper()
	r, err := OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer func() {
		if cerr := r.Close(); cerr != nil {
			t.Errorf("reader Close: %v", cerr)
		}
	}()
	var tags []string
	for f := range r.Frames() {
		tags = append(tags, tagOf(f.Payload))
	}
	return tags
}

// longestRun returns the length of the longest run of consecutive frames
// carrying tag, and how many separate runs of it there are.
//
// A transaction whose frames are contiguous appears as exactly ONE run. Two or
// more runs means another appender's frame landed inside it, which is precisely
// what makes recovery's suffix filter drop committed data.
func runsOf(tags []string, tag string) (runs, longest int) {
	cur := 0
	for _, got := range tags {
		if got == tag {
			cur++
			if cur > longest {
				longest = cur
			}
			continue
		}
		if cur > 0 {
			runs++
		}
		cur = 0
	}
	if cur > 0 {
		runs++
	}
	return runs, longest
}

// appendContiguity drives one transaction of frameCount frames against a WAL
// while `competitors` goroutines append their own frames as fast as they can, and
// reports how many separate runs the transaction ended up as.
//
// useRun selects the arm: AppendRun (the fix) or a loop of individual Append
// calls (what the code did before, and what any future caller that reaches for
// Append in a loop will get).
func appendContiguity(t *testing.T, useRun bool, frameCount, competitors int) (runs, longest, total int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wal")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	frames := txnFrames("T", frameCount)
	// start releases every goroutine at once, so the competitors are already
	// inside their append loops when the transaction begins.
	start := make(chan struct{})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for c := 0; c < competitors; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			<-start
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if aerr := w.Append([]byte(fmt.Sprintf("C%d:op%d", c, i))); aerr != nil {
					t.Errorf("competitor %d Append: %v", c, aerr)
					return
				}
			}
		}(c)
	}

	close(start)
	// Let the competitors get going, so the transaction is genuinely appending
	// into a contended writer rather than into a quiet one.
	for i := 0; i < 200; i++ {
		if aerr := w.Append([]byte("W:warm")); aerr != nil {
			t.Fatalf("warm Append: %v", aerr)
		}
	}

	if useRun {
		if _, aerr := w.AppendRun(func(emit func([]byte) error) error {
			for _, p := range frames {
				if err := emit(p); err != nil {
					return err
				}
			}
			return nil
		}); aerr != nil {
			t.Fatalf("AppendRun: %v", aerr)
		}
	} else {
		for _, p := range frames {
			if aerr := w.Append(p); aerr != nil {
				t.Fatalf("Append: %v", aerr)
			}
		}
	}

	close(stop)
	wg.Wait()
	if serr := w.Sync(); serr != nil {
		t.Fatalf("Sync: %v", serr)
	}
	if cerr := w.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}

	tags := readTags(t, path)
	runs, longest = runsOf(tags, "T")
	return runs, longest, len(frames)
}

// TestAppendRun_KeepsATransactionContiguous is the acceptance instrument.
//
// The transaction must appear as exactly ONE run of exactly its own frame count,
// however hard other appenders push. Anything else is a WAL from which recovery
// cannot reconstruct the transaction: its suffix filter walks forward to the
// first frame carrying the marker's TxnSeq and commits from there, so a foreign
// frame in the middle either fuses another transaction's op into this one or
// leaves this one's earlier ops behind.
func TestAppendRun_KeepsATransactionContiguous(t *testing.T) {
	const frames = 24
	runs, longest, total := appendContiguity(t, true, frames, 8)
	if runs != 1 || longest != total {
		t.Fatalf("AppendRun produced %d runs, longest %d of %d frames: the transaction's "+
			"frames interleaved with another appender's, which is exactly what recovery's "+
			"contiguity assumption cannot survive", runs, longest, total)
	}
}

// TestAppend_LoopInterleavesUnderConcurrency is the negative arm, and it is what
// gives the test above meaning.
//
// It asserts the defect: a loop of individual Append calls DOES interleave under
// concurrent appenders. If this ever stops being true — because Append grew a
// hold-across-calls behaviour, or because the competitors stopped competing — then
// the positive test is no longer measuring anything and this failure says so.
func TestAppend_LoopInterleavesUnderConcurrency(t *testing.T) {
	const frames = 24
	// Contention is scheduler-dependent, so allow several attempts before
	// concluding the interleaving cannot be observed at all. One observation is
	// enough: it proves the loop offers no contiguity guarantee.
	for attempt := 0; attempt < 5; attempt++ {
		runs, longest, total := appendContiguity(t, false, frames, 8)
		if runs != 1 || longest != total {
			t.Logf("attempt %d: the append loop produced %d runs, longest %d of %d frames — "+
				"the defect reproduced", attempt, runs, longest, total)
			return
		}
	}
	t.Fatalf("a loop of %d individual Appends stayed contiguous across 5 attempts against 8 "+
		"concurrent appenders. Either Append now serialises runs (in which case AppendRun's "+
		"contiguity test proves nothing and this whole file needs rethinking) or the "+
		"competitors are not actually contending", frames)
}

// TestAppendRun_PropagatesTheCallbackError covers the failure path: a run that
// aborts mid-way leaves the frames it did append in the buffer, un-marked, which
// recovery discards for atomicity exactly as it discards a crash between the data
// frames and the commit marker.
func TestAppendRun_PropagatesTheCallbackError(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "wal")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sentinel := fmt.Errorf("encode failed")
	_, rerr := w.AppendRun(func(emit func([]byte) error) error {
		if aerr := emit([]byte("T:op0")); aerr != nil {
			return aerr
		}
		return sentinel
	})
	if rerr != sentinel { //nolint:errorlint // identity is the assertion
		t.Fatalf("AppendRun returned %v, want the callback's own error unchanged", rerr)
	}
	if serr := w.Sync(); serr != nil {
		t.Fatalf("Sync: %v", serr)
	}
	if cerr := w.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
	// The partial run is on disk and un-marked. That is the correct outcome: it is
	// indistinguishable from a crash before the marker, and recovery already
	// discards such a tail.
	if got := readTags(t, path); len(got) != 1 || got[0] != "T" {
		t.Fatalf("frames after an aborted run = %v, want exactly the one appended before "+
			"the error", got)
	}
}

// TestAppendRun_RejectsUseAfterTheRunReturns pins the one hazard the callback
// form introduces: an escaped closure would write into a writer its goroutine no
// longer holds.
func TestAppendRun_RejectsUseAfterTheRunReturns(t *testing.T) {
	t.Parallel()
	w, err := Open(filepath.Join(t.TempDir(), "wal"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if cerr := w.Close(); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	}()

	var escaped func([]byte) error
	if _, aerr := w.AppendRun(func(emit func([]byte) error) error {
		escaped = emit
		return nil
	}); aerr != nil {
		t.Fatalf("AppendRun: %v", aerr)
	}
	defer func() {
		if recover() == nil {
			t.Error("using the append closure after the run returned did not panic; an " +
				"escaped closure writes into a writer this goroutine does not hold")
		}
	}()
	_ = escaped([]byte("T:late"))
}
