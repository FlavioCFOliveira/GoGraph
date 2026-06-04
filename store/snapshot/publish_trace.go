package snapshot

import "sync/atomic"

// publishTraceHook is a test-only observation seam for the snapshot
// publish protocol's crash-safety ordering. It holds nil in production —
// the only cost on the publish path is one atomic load plus a nil
// comparison per event, the same cost class as the metrics counters that
// already instrument this path — and is set by tests that need to assert
// the canonical crash-safe ordering:
//
//	write+fsync each component -> fsync staging dir -> rename -> fsync parent
//
// The writers invoke it with one of the following (event, path) pairs,
// in the order the corresponding step executes:
//
//	("staging-fsync", tmp) — emitted immediately after the STAGING
//	    directory's own inode has been successfully fsync'd via dirFsync,
//	    and BEFORE the publish rename. This is the event whose ABSENCE
//	    proves the durability defect: a writer that fsyncs only the
//	    components and the grandparent never fsyncs the staging dir, so
//	    this event would never fire.
//
//	("rename", tmp) — emitted immediately before os.Rename(tmp, dir)
//	    publishes the staging directory.
//
// It is an [atomic.Pointer] rather than a plain variable so that a
// concurrent publisher in another test (the package publishes snapshots
// from several parallel tests) reads it race-free while a serial test has
// a recorder installed. The installing test resets it to nil on cleanup
// so it never leaks across tests; it must only be set from test code.
var publishTraceHook atomic.Pointer[func(event, path string)]

// notePublishStep invokes the installed publish-trace hook when one is
// set. It is a no-op in production (the pointer is nil). Kept tiny so the
// publish path pays only an atomic load plus a nil check when tracing is
// disabled.
func notePublishStep(event, path string) {
	if h := publishTraceHook.Load(); h != nil {
		(*h)(event, path)
	}
}
