package main

// durability_test.go — the short-layer gate of phase 7, durability under
// concurrent writers across a crash (rmp #2935): the in-process arms at 8 and 64
// writers, and the D14 negative control.
//
// Layer: short. The kill -9 arm (D02) is in durability_soak_test.go.

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// durabilityKeys lists every check a complete in-process run must have printed.
var durabilityKeys = []string{
	"D01.writers_running_at_crash", "D01.no_unexpected_errors", "D11.ddl_ran",
	"D01.durable.acked_present", "D01.durable.refused_absent", "D01.durable.whole_or_absent",
	"D01.durable.counters_conserved", "D01.durable.open_absent", "D01.durable.clock_not_rewound",
	"D01.durable.blobs_identical", "D01.durable.no_hole", "D01.durable.wal_tags_seen",
	"D01.durable.new_session_sees_acked", "D01.durable.seek_equals_scan", "D01.durable.unique_holds",
	"D01.durable.post_recovery_commit_is_new",
	"D01.written.acked_present", "D01.written.whole_or_absent", "D01.written.seek_equals_scan",
	"D03.durable_but_unacknowledged",
	"D08.reference.acked_present", "D08.torn.damaged_record_discarded_alone",
	"D08.garbled.damaged_record_discarded_alone", "D08.garbled.refused_unless_clean",
	"D13.double_recovery_identical",
	"D09.checkpoint_ran", "D09.pre_capture.acked_present", "D09.pre_truncate.acked_present",
	"D09.post_truncate.acked_present", "D09.post_truncate.seek_equals_scan",
	"D16.snapshot_used", "D16.checkpoint_plus_tail_equals_full_replay",
	"D09.missing_segment.refused_loudly", "D09.checkpoint_refused_not_quiesced", "D09.capture_not_refused",
	"D04.failure_seen", "D04.post_poison_commit_refused", "D04.failed_not_visible",
	"D04.reopen.acked_present", "D04.reopen.refused_absent", "D04.recovers_exactly_the_acknowledged",
}

// runDurability runs phase 7 and holds it to every check and every key.
func runDurability(t *testing.T, dc *durabilityConfig, keys []string) {
	t.Helper()
	var buf bytes.Buffer
	out, err := phaseDurability(context.Background(), &buf, dc)
	if err != nil {
		t.Fatalf("durability: %v\n%s", err, buf.String())
	}
	for _, f := range out.failed() {
		t.Error(f)
	}
	for _, k := range keys {
		if !out.has(k) {
			t.Errorf("phase 7 never emitted %s", k)
		}
	}
	if t.Failed() || testing.Verbose() {
		t.Log("\n" + buf.String())
	}
}

// TestDurability is the short layer: every in-process arm at 8 and 64 writers.
func TestDurability(t *testing.T) {
	dc := defaultDurabilityConfig()
	runDurability(t, &dc, durabilityKeys)
}

// TestDurabilityNegativeControl is D14: the seam drops the last acknowledged
// commit from the crash image, and the "acknowledged present" gate must fail.
// The seam is off in every other run.
func TestDurabilityNegativeControl(t *testing.T) {
	dc := defaultDurabilityConfig()
	dc.dropLastAcked = true
	var buf bytes.Buffer
	out := newPhaseOut(&buf, "durability")
	if err := armAbandon(context.Background(), &dc, out, 8); err != nil {
		t.Fatalf("abandon arm: %v\n%s", err, buf.String())
	}
	found := false
	for _, f := range out.failed() {
		if strings.HasPrefix(f, "durability.D01.durable level=8 acked_present:") {
			found = true
		}
	}
	if !found {
		t.Errorf("with the seam dropping the last acknowledged WAL record, acked_present did not fail: "+
			"the check cannot detect a lost commit\n%s", buf.String())
	}
	if testing.Verbose() {
		t.Log("\n" + buf.String())
	}
}
