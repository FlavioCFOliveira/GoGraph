package cypher

// index_snapshot_seams_test.go — the two rmp #2937 seams the regression tests in
// index_snapshot_read_test.go reach through, kept apart so those tests read the
// same whichever build they run against.

// setTrustIndexSnapshot turns the rmp #2937 proof off (on = true) or back on for
// every plan e builds from now on: the negative control.
func setTrustIndexSnapshot(e *Engine, on bool) { e.trustIndexSnapshotForTest = on }

// indexSnapshotDeclines returns the process-wide count of index reads a MATCH
// access path discarded for want of the proof.
func indexSnapshotDeclines() uint64 { return indexSnapshotDeclineCount.Load() }
