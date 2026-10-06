package main

// ladder_test.go — the short-layer gate of phase 6, the concurrency ladder
// (rmp #2934): every arm at 1, 8 and 64 goroutines.
//
// Layer: short. The soak layer (ladder_soak_test.go) runs 256 and 1024.

import (
	"bytes"
	"context"
	"testing"
)

// samplerKeys are the frontier and growth counters every sampled arm emits.
var samplerKeys = []string{
	"peak_in_flight_commits", "peak_sessions_waiting", "out_of_order_publications",
	"helped_publications", "max_visibility_lag", "peak_versions_total",
	"peak_unregistered_snapshots", "watermark_regressions", "horizon_stale_leaves",
	"snapshot_capacity", "conflicts_by_store", "in_flight_commits_end",
	"quiesced_versions_total",
}

// txKeys are the operation counters every writer arm emits.
var txKeys = []string{"commits_per_sec", "refused_attempts", "longest_retry_streak", "longest_retry_streak_time", "unrecovered"}

// requiredKeys lists every counter a complete ladder run must have printed.
func requiredKeys(lc *ladderConfig) []string {
	sampled := []string{"L01", "L04.sessionless", "L04.session", "L05", "L06", "L08",
		"L10.sessionless", "L10.session", "L11.sessionless", "L11.session",
		"L13", "L13.memory", "MG11", "L14", "L15", "L18", "L19"}
	writers := []string{"L01", "L04.sessionless", "L04.session", "L05", "L06", "L08",
		"L10.sessionless", "L10.session", "L11.sessionless", "L11.session", "L15", "L18", "L19"}
	var keys []string
	for _, r := range sampled {
		for _, k := range samplerKeys {
			keys = append(keys, r+"."+k)
		}
	}
	for _, r := range writers {
		for _, k := range txKeys {
			keys = append(keys, r+"."+k)
		}
	}
	for _, r := range []string{"L10.sessionless", "L10.session", "L11.sessionless", "L11.session"} {
		keys = append(keys, r+".self_conflicts", r+".longest_self_conflict_streak", r+".longest_self_conflict_streak_time")
	}
	keys = append(keys,
		"L01.forbidden", "L01.permitted_g2_item", "L02.write_skew_permitted", "L03.repeatable_reads",
		"L05.large_longest_refused_streak", "L06.seek_scan_mismatches", "L06.seek_scan_first_mismatches", "L06.seek_equals_scan", "L07.own_seek_equals_scan",
		"L08.versions_held", "L08.retention_shown", "L10.sessionless.store_bytes_after",
		"L10.sessionless.commit_p99_during_checkpoint", "L13.duplicates", "L13.memory.failed_callers", "L14.duplicates",
		"L15.dangling_arcs", "L16.traversal_repeatable", "L17.cancel_return_lag", "L18.dangling_arcs",
		"L19.ddl_max_latency", "mem.heap_alloc_bytes")
	if lc.soak {
		keys = append(keys, "L09.unregistered_snapshots", "L09.past_capacity_unregistered",
			"L10.sessionless.self_streak_below_budget", "L11.sessionless.self_streak_below_budget")
	}
	return keys
}

// runLadder runs the ladder and holds it to every check and every counter.
func runLadder(t *testing.T, lc *ladderConfig) {
	t.Helper()
	var buf bytes.Buffer
	out, err := phaseLadder(context.Background(), &buf, lc)
	if err != nil {
		t.Fatalf("ladder: %v\n%s", err, buf.String())
	}
	for _, f := range out.failed() {
		t.Error(f)
	}
	if len(lc.rows) == 0 {
		for _, k := range requiredKeys(lc) {
			if !out.has(k) {
				t.Errorf("the ladder never emitted %s", k)
			}
		}
	}
	if t.Failed() || testing.Verbose() {
		t.Log("\n" + buf.String())
	}
}

// TestLadder is the short layer: 1, 8 and 64 goroutines.
func TestLadder(t *testing.T) {
	lc := defaultLadderConfig()
	runLadder(t, &lc)
}
