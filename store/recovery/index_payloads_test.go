package recovery

// index_payloads_test.go — what recovery REPORTS about a snapshot's
// secondary-index payloads (rmp #2490): the per-payload reason codes, the
// supported lookup, and the WAL-suffix facts a caller needs to decide staleness
// per index.
//
// The behavioural half — an index actually hydrated instead of rebuilt, and the
// rows a seek then returns — lives in package cypher, which owns the index
// bindings and the name-to-(label, property) mapping. Here the subject is
// strictly the facts recovery hands over.
//
// Layer: short.

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// ─── indexImageReason: the two whole-image preconditions ────────────────────

func TestIndexImageReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		selfSufficient  bool
		indexesCommitTS uint64
		wantErr         error
	}{
		{"self-sufficient and watermarked", true, 7, nil},
		{"not self-sufficient", false, 7, ErrIndexPayloadStale},
		{"no watermark", true, 0, ErrIndexPayloadStale},
		{"neither", false, 0, ErrIndexPayloadStale},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := indexImageReason(tc.selfSufficient, tc.indexesCommitTS)
			if tc.wantErr == nil {
				if got != nil {
					t.Fatalf("indexImageReason = %v, want nil", got)
				}
				return
			}
			if !errors.Is(got, tc.wantErr) {
				t.Fatalf("indexImageReason = %v, want %v", got, tc.wantErr)
			}
		})
	}
	// The two refusals must be DISTINGUISHABLE in their message, or an operator
	// cannot tell a mapper-less image from an unwatermarked one.
	noMapper := indexImageReason(false, 7).Error()
	noWatermark := indexImageReason(true, 0).Error()
	if noMapper == noWatermark {
		t.Fatalf("both refusals produced the identical message %q", noMapper)
	}
}

// ─── classifyIndexPayloads: precedence and the hydratable count ─────────────

func TestClassifyIndexPayloads(t *testing.T) {
	t.Parallel()

	rb := []snapshot.IndexReadback{
		{Name: "healthy", Bytes: []byte{1, 2, 3}},
		{Name: "damaged", Bytes: nil},
	}

	t.Run("hydratable image", func(t *testing.T) {
		t.Parallel()
		out, n := classifyIndexPayloads(rb, nil)
		if n != 1 {
			t.Fatalf("hydratable = %d, want 1", n)
		}
		if len(out) != 2 {
			t.Fatalf("payloads = %d, want 2", len(out))
		}
		if out[0].Name != "healthy" || out[0].Err != nil || len(out[0].Bytes) != 3 {
			t.Fatalf("healthy payload = %+v, want 3 bytes and no error", out[0])
		}
		if !errors.Is(out[1].Err, ErrIndexPayloadUnreadable) {
			t.Fatalf("damaged payload Err = %v, want ErrIndexPayloadUnreadable", out[1].Err)
		}
		if out[1].Bytes != nil {
			t.Fatalf("damaged payload must hand out no bytes, got %d", len(out[1].Bytes))
		}
		// The name must travel with the reason: a bare sentinel cannot be
		// attributed to an index by an operator reading a log line.
		if !strings.Contains(out[1].Err.Error(), `"damaged"`) {
			t.Fatalf("damaged payload Err %q does not name the index", out[1].Err)
		}
	})

	t.Run("image reason wins over per-payload state", func(t *testing.T) {
		t.Parallel()
		reason := indexImageReason(false, 0)
		out, n := classifyIndexPayloads(rb, reason)
		if n != 0 {
			t.Fatalf("hydratable = %d, want 0 for an unhydratable image", n)
		}
		for i := range out {
			if !errors.Is(out[i].Err, ErrIndexPayloadStale) {
				t.Fatalf("payload %q Err = %v, want ErrIndexPayloadStale — the image reason must "+
					"win, including over an unreadable payload whose unreadability is not what "+
					"stops the caller using it", out[i].Name, out[i].Err)
			}
			if out[i].Bytes != nil {
				t.Fatalf("payload %q handed out bytes for an unhydratable image", out[i].Name)
			}
		}
	})

	t.Run("no readbacks", func(t *testing.T) {
		t.Parallel()
		out, n := classifyIndexPayloads(nil, nil)
		if out != nil || n != 0 {
			t.Fatalf("classifyIndexPayloads(nil) = (%v, %d), want (nil, 0)", out, n)
		}
	})
}

// ─── IndexPayloadFor: the supported lookup ──────────────────────────────────

func TestResult_IndexPayloadFor(t *testing.T) {
	t.Parallel()
	res := Result[string, float64]{
		SnapshotIndexPayloads: []IndexPayload{
			{Name: "ok", Bytes: []byte{9}},
			{Name: "bad", Err: ErrIndexPayloadUnreadable},
		},
	}
	if b, err := res.IndexPayloadFor("ok"); err != nil || len(b) != 1 || b[0] != 9 {
		t.Fatalf("IndexPayloadFor(ok) = (%v, %v), want ([9], nil)", b, err)
	}
	if b, err := res.IndexPayloadFor("bad"); b != nil || !errors.Is(err, ErrIndexPayloadUnreadable) {
		t.Fatalf("IndexPayloadFor(bad) = (%v, %v), want (nil, ErrIndexPayloadUnreadable)", b, err)
	}
	// An index the snapshot never declared is NOT reported as damaged: the two
	// events are different and only one deserves a corruption metric.
	b, err := res.IndexPayloadFor("never-captured")
	if b != nil || !errors.Is(err, ErrIndexPayloadNotFound) {
		t.Fatalf("IndexPayloadFor(never-captured) = (%v, %v), want (nil, ErrIndexPayloadNotFound)", b, err)
	}
	if errors.Is(err, ErrIndexPayloadUnreadable) || errors.Is(err, ErrIndexPayloadStale) {
		t.Fatalf("an absent payload must not report as unreadable or stale: %v", err)
	}
	// A zero Result answers the same way rather than panicking.
	var zero Result[string, float64]
	if _, err := zero.IndexPayloadFor("anything"); !errors.Is(err, ErrIndexPayloadNotFound) {
		t.Fatalf("zero Result IndexPayloadFor = %v, want ErrIndexPayloadNotFound", err)
	}
}

// ─── WALSuffixTouchesNodeIndex: the per-index staleness predicate ────────────

func TestResult_WALSuffixTouchesNodeIndex(t *testing.T) {
	t.Parallel()
	res := Result[string, float64]{
		WALTouchedNodeLabels:       []string{"Company", "Person"},
		WALTouchedNodePropertyKeys: []string{"age", "name"},
	}
	tests := []struct {
		label, property string
		want            bool
	}{
		{"Person", "name", true},  // both touched
		{"Person", "city", true},  // label touched only
		{"City", "name", true},    // property touched only
		{"City", "zip", false},    // neither
		{"person", "Name", false}, // case-sensitive: not the same facet
	}
	for _, tc := range tests {
		if got := res.WALSuffixTouchesNodeIndex(tc.label, tc.property); got != tc.want {
			t.Errorf("WALSuffixTouchesNodeIndex(%q, %q) = %v, want %v", tc.label, tc.property, got, tc.want)
		}
	}
	// A Result with no recorded facets can never refuse a hydration — which is
	// the state a freshly checkpointed directory is in, and the whole point.
	var zero Result[string, float64]
	if zero.WALSuffixTouchesNodeIndex("Person", "name") {
		t.Fatal("a Result with no touched facets must report no index as stale")
	}
}

// ─── the touched sets, collected through a REAL Open ────────────────────────

// walTouchOpts is the codec pair every case below recovers with.
func walTouchOpts() Options[string, float64] {
	return Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
}

// writeWAL commits fn's ops through a real store into dir/wal and closes it, so
// the subsequent Open replays exactly what a crash would have left behind.
func writeWAL(t *testing.T, dir string, fn func(tx *txn.Tx[string, float64])) {
	t.Helper()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	st := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	tx := st.Begin()
	fn(tx)
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
}

// TestOpen_ReportsTheNodeFacetsTheWALTouched pins the facts behind the
// per-index staleness gate. Each case commits one op shape and asserts the
// EXACT reported sets, so an op wrongly classified as index-neutral (or wrongly
// classified as relevant) fails here rather than silently widening or narrowing
// the gate downstream.
//
// The `edges and bare nodes` case is the load-bearing negative control: without
// it every other case would pass against an implementation that simply reported
// every label and key it ever saw, and a gate that always refuses hydration is
// indistinguishable from having no gate at all.
func TestOpen_ReportsTheNodeFacetsTheWALTouched(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		commit     func(tx *txn.Tx[string, float64])
		wantLabels []string
		wantKeys   []string
	}{
		{
			name: "node label set and removed",
			commit: func(tx *txn.Tx[string, float64]) {
				must(t, tx.SetNodeLabel("a", "Person"))
				must(t, tx.SetNodeLabel("b", "Company"))
				must(t, tx.RemoveNodeLabel("b", "Company"))
			},
			wantLabels: []string{"Company", "Person"},
		},
		{
			name: "node property written and deleted",
			commit: func(tx *txn.Tx[string, float64]) {
				must(t, tx.SetNodeProperty("a", "name", lpg.StringValue("Alice")))
				must(t, tx.SetNodeProperty("a", "age", lpg.Int64Value(30)))
				must(t, tx.DelNodeProperty("a", "age"))
			},
			wantKeys: []string{"age", "name"},
		},
		{
			name: "node removal contributes its labels AND its keys",
			commit: func(tx *txn.Tx[string, float64]) {
				must(t, tx.SetNodeLabel("a", "Person"))
				must(t, tx.SetNodeProperty("a", "name", lpg.StringValue("Alice")))
				must(t, tx.RemoveNode("a"))
			},
			wantLabels: []string{"Person"},
			wantKeys:   []string{"name"},
		},
		{
			name: "edges and bare nodes are index-neutral",
			commit: func(tx *txn.Tx[string, float64]) {
				must(t, tx.AddNode("a"))
				must(t, tx.AddNode("b"))
				must(t, tx.AddEdge("a", "b", 1.5))
				must(t, tx.SetEdgeLabel("a", "b", "KNOWS"))
			},
		},
		{
			name: "index DDL is index-neutral for the facet sets",
			commit: func(tx *txn.Tx[string, float64]) {
				must(t, tx.AddNode("a"))
				must(t, tx.CreateIndex(txn.IndexKindHash, "Person", "name", "idx_person_name"))
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeWAL(t, dir, tc.commit)
			res, err := Open[string, float64](dir, walTouchOpts())
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if res.WALOps == 0 {
				t.Fatal("WALOps = 0: the case committed nothing, so its facet assertions are vacuous")
			}
			if !reflect.DeepEqual(res.WALTouchedNodeLabels, tc.wantLabels) {
				t.Errorf("WALTouchedNodeLabels = %q, want %q", res.WALTouchedNodeLabels, tc.wantLabels)
			}
			if !reflect.DeepEqual(res.WALTouchedNodePropertyKeys, tc.wantKeys) {
				t.Errorf("WALTouchedNodePropertyKeys = %q, want %q", res.WALTouchedNodePropertyKeys, tc.wantKeys)
			}
		})
	}
}

// TestReplayWAL_ReportsTheSameFacets pins that the reusable WAL-only replay core
// — the one the deterministic simulation harness drives — reports the identical
// facts as the snapshot+WAL path, so a scenario recovered through either entry
// point can apply the same staleness gate.
func TestReplayWAL_ReportsTheSameFacets(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWAL(t, dir, func(tx *txn.Tx[string, float64]) {
		must(t, tx.SetNodeLabel("a", "Person"))
		must(t, tx.SetNodeProperty("a", "name", lpg.StringValue("Alice")))
	})

	r, err := wal.OpenReader(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.OpenReader: %v", err)
	}
	defer func() { _ = r.Close() }()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	rr, err := ReplayWAL[string, float64](t.Context(), r, g,
		txn.NewStringCodec(), txn.NewFloat64WeightCodec(), 0)
	if err != nil {
		t.Fatalf("ReplayWAL: %v", err)
	}
	if rr.WALOps == 0 {
		t.Fatal("WALOps = 0: nothing replayed, so the assertions below are vacuous")
	}
	if want := []string{"Person"}; !reflect.DeepEqual(rr.WALTouchedNodeLabels, want) {
		t.Errorf("WALTouchedNodeLabels = %q, want %q", rr.WALTouchedNodeLabels, want)
	}
	if want := []string{"name"}; !reflect.DeepEqual(rr.WALTouchedNodePropertyKeys, want) {
		t.Errorf("WALTouchedNodePropertyKeys = %q, want %q", rr.WALTouchedNodePropertyKeys, want)
	}
}

// TestOpen_NoSnapshotReportsNoPayloads covers the ordinary fresh-directory
// shape: no snapshot means no payloads, no hydratable count, and an image that
// is not self-sufficient (there is no image).
func TestOpen_NoSnapshotReportsNoPayloads(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWAL(t, dir, func(tx *txn.Tx[string, float64]) {
		must(t, tx.SetNodeLabel("a", "Person"))
	})
	res, err := Open[string, float64](dir, walTouchOpts())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if res.SnapshotHit {
		t.Fatal("SnapshotHit = true for a directory with no snapshot")
	}
	if res.SnapshotSelfSufficient {
		t.Fatal("SnapshotSelfSufficient = true with no snapshot at all")
	}
	if res.SnapshotIndexes != 0 || res.SnapshotIndexPayloads != nil {
		t.Fatalf("SnapshotIndexes/Payloads = %d/%v, want 0/nil", res.SnapshotIndexes, res.SnapshotIndexPayloads)
	}
	if _, err := res.IndexPayloadFor("anything"); !errors.Is(err, ErrIndexPayloadNotFound) {
		t.Fatalf("IndexPayloadFor = %v, want ErrIndexPayloadNotFound", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
