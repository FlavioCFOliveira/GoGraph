package csrfile

import "sync/atomic"

// publishTraceHook is a test-only observation seam for the [WriteToFile]
// publish protocol's crash-safety ordering. It holds nil in production —
// the only cost on the publish path is one atomic load plus a nil
// comparison per event — and is set by tests that need to assert the
// canonical crash-safe ordering:
//
//	write+fsync temp file -> rename -> fsync parent dir
//
// [WriteToFile] invokes it with one of the following (event, path) pairs,
// in the order the corresponding step executes:
//
//	("rename", path) — emitted immediately after os.Rename(tmp, path)
//	    publishes the temp file onto its final path.
//
//	("parent-fsync", path) — emitted immediately after the PARENT
//	    directory's own inode has been successfully fsync'd via
//	    parentDirFsync, making the rename's directory entry durable. This
//	    is the event whose ABSENCE proves the durability defect: a writer
//	    that fsyncs only the temp file and renames it never fsyncs the
//	    parent, so this event would never fire.
//
// It is an [atomic.Pointer] rather than a plain variable so that a
// concurrent writer in another test reads it race-free while a serial
// test has a recorder installed. The installing test resets it to nil on
// cleanup so it never leaks across tests; it must only be set from test
// code.
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
