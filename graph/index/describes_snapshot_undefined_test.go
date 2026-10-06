package index

import "testing"

// TestDescribesSnapshot_FalseOnceUndefined pins that an index whose commit-time
// delivery was cut short describes no snapshot. A panic in the delivery's first
// step never raises the applying counter, so without this rule
// DescribesSnapshot kept answering true (rmp #2936 audit, L1).
func TestDescribesSnapshot_FalseOnceUndefined(t *testing.T) {
	m := NewManager()
	if !m.DescribesSnapshot(0) {
		t.Fatal("fixture: an idle manager does not describe snapshot 0")
	}
	m.MarkUndefined("test: CommitApplyShards panicked")
	for _, ts := range []uint64{0, 1, 1 << 40} {
		if m.DescribesSnapshot(ts) {
			t.Fatalf("DescribesSnapshot(%d) is true with the index state undefined", ts)
		}
	}
}
