package sim

// bolt_stream_semantics_test.go — the tests for the Bolt streaming arms
// (rmp #2484).
//
// The shape follows this sprint's three completed tasks
// (bolt_auth_surface_test.go, bolt_tx_registry_test.go,
// bolt_shutdown_drain_test.go): every clause of BOTH adjudicators is falsified by
// perturbing ONE field of a hand-built healthy evidence value, and the controls
// drive REAL servers rather than doctored structs. goleak.VerifyNone runs in every
// test, because a stream abandoned mid-flight is exactly the shape that leaks a
// parked writer goroutine.

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// boltStreamTestSeeds are the seeds the arms are driven at. Arbitrary but FIXED,
// so a failure is reproducible from the test log alone.
var boltStreamTestSeeds = []uint64{0x2484_5701, 0x2484_0002, 0x2484_0003, 7}

// boltStreamArmTimeout bounds one run. A run takes well under a second in
// practice; the bound exists so a regression that stalls a drain fails the test
// instead of hanging the package.
const boltStreamArmTimeout = 60 * time.Second

// -----------------------------------------------------------------------------
// The arms, driven for real
// -----------------------------------------------------------------------------

// TestBoltStreamSemantics_Arms drives the deterministic battery at several seeds
// and requires each run to satisfy BOTH its contract and its non-vacuity gate.
func TestBoltStreamSemantics_Arms(t *testing.T) {
	defer goleak.VerifyNone(t)
	for _, seed := range boltStreamTestSeeds {
		ctx, cancel := context.WithTimeout(context.Background(), boltStreamArmTimeout)
		ev, err := RunBoltStreamSemantics(ctx, seed)
		cancel()
		if err != nil {
			t.Fatalf("seed %#x: %v", seed, err)
		}
		if v := checkBoltStreamSemantics(ev); len(v) > 0 {
			t.Fatalf("seed %#x contract violations %v\n%s", seed, v, ev)
		}
		if v := checkBoltStreamSemanticsNonVacuity(ev); len(v) > 0 {
			t.Fatalf("seed %#x coverage shortfall %v\n%s", seed, v, ev)
		}
	}
}

// TestBoltStreamSemantics_Measurements pins the figures the oracles rest on, so a
// change that quietly stopped paging — or stopped accumulating cursors, or stopped
// reaching the WAL — fails here rather than passing an oracle with nothing to
// adjudicate.
func TestBoltStreamSemantics_Measurements(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx, cancel := context.WithTimeout(context.Background(), boltStreamArmTimeout)
	defer cancel()
	ev, err := RunBoltStreamSemantics(ctx, boltStreamTestSeeds[0])
	if err != nil {
		t.Fatalf("RunBoltStreamSemantics: %v", err)
	}

	// The reference drain and the paged drain must agree on the row count, and the
	// paged one must genuinely have paged. The minimum page count is arithmetic: the
	// largest drawable page is boltStreamMaxPage, so draining boltStreamRows rows
	// takes at least ceil(97/16) = 7 pages whatever the seed draws.
	const minPages = (boltStreamRows + boltStreamMaxPage - 1) / boltStreamMaxPage
	if len(ev.Reference) != boltStreamRows {
		t.Errorf("reference rows = %d, want %d", len(ev.Reference), boltStreamRows)
	}
	if len(ev.Paged) != boltStreamRows {
		t.Errorf("paged rows = %d, want %d", len(ev.Paged), boltStreamRows)
	}
	if len(ev.Pages) < minPages {
		t.Errorf("pages = %d, want at least %d", len(ev.Pages), minPages)
	}
	if len(ev.Reference[0]) != len(boltStreamColumns) {
		t.Errorf("reference row width = %d, want %d", len(ev.Reference[0]), len(boltStreamColumns))
	}

	// The has_more distribution: exactly one false (the final page), the rest true.
	trues, falses := 0, 0
	for i := range ev.Pages {
		if ev.Pages[i].HasMore {
			trues++
		} else {
			falses++
		}
	}
	if falses != 1 || trues != len(ev.Pages)-1 {
		t.Errorf("has_more distribution: %d true / %d false over %d pages, want %d/1",
			trues, falses, len(ev.Pages), len(ev.Pages)-1)
	}
	t.Logf("paging: plan %v -> %d pages, has_more %d true / %d false",
		ev.PageSizes, len(ev.Pages), trues, falses)

	// The discard window must be strictly interior, and the DISCARD must have
	// delivered nothing.
	k, d := len(ev.WindowPrefix), int(ev.WindowDiscardN)
	if k == 0 || k+d >= boltStreamRows {
		t.Errorf("discard window not interior: prefix %d + n %d against %d rows", k, d, boltStreamRows)
	}
	if len(ev.WindowSuffix) != boltStreamRows-k-d {
		t.Errorf("window suffix = %d row(s), want %d", len(ev.WindowSuffix), boltStreamRows-k-d)
	}
	t.Logf("window: prefix %d row(s) over %d page(s), DISCARD n=%d, suffix %d row(s), has_more=%v",
		k, ev.WindowPrefixPages, d, len(ev.WindowSuffix), ev.WindowDiscard.HasMore)

	// The qid census: every RUN reply reports -1, and there are many of them.
	if got := boltStreamDistinct(ev.RunQIDs); len(got) != 1 || got[0] != -1 {
		t.Errorf("distinct RUN qids = %v, want exactly [-1]", got)
	}
	if len(ev.RunQIDs) < boltStreamMinRunQIDs {
		t.Errorf("RUN qid readings = %d, want at least %d", len(ev.RunQIDs), boltStreamMinRunQIDs)
	}
	t.Logf("qid: %d RUN replies inspected, distinct %v; probed %v; control delivered %d row(s)",
		len(ev.RunQIDs), boltStreamDistinct(ev.RunQIDs), ev.ProbedQIDs, ev.QIDControlRows)

	// The in-flight arms: the committed one reached the WAL, the doomed one did not.
	under := boltStreamTxArm(ev, "cursors-under-cap")
	over := boltStreamTxArm(ev, "cursors-over-cap")
	if under == nil || over == nil {
		t.Fatalf("in-flight arms missing: %+v", ev.TxArms)
	}
	if under.framesAppended() == 0 {
		t.Error("the committed transaction appended no WAL frame: the counter is not a live instrument")
	}
	if over.framesAppended() != 0 || over.bytesAppended() != 0 {
		t.Errorf("the doomed transaction appended frames+%d bytes+%d, want 0/0",
			over.framesAppended(), over.bytesAppended())
	}
	if over.CursorsAtRefusal != boltStreamInFlightCap {
		t.Errorf("the refusal reported open=%d, want %d", over.CursorsAtRefusal, boltStreamInFlightCap)
	}
	t.Logf("inflight: under-cap cycles=%d committed=%v frames+%d | over-cap cycles=%d open=%d frames+%d bytes+%d code=%q",
		under.Cycles, under.Committed, under.framesAppended(),
		over.Cycles, over.CursorsAtRefusal, over.framesAppended(), over.bytesAppended(), over.RefusalCode)
	t.Logf("census: committed live=%d recovered=%d | doomed live=%d recovered=%d | discarded live=%d recovered=%d",
		ev.CommittedLive, ev.CommittedRecovered, ev.DoomedLive, ev.DoomedRecovered,
		ev.EffectLive, ev.EffectRecovered)
	t.Logf("effect stats: {%s}", boltStreamRenderStats(ev.EffectStats))
}

// TestBoltStreamSemantics_Determinism requires two runs of one seed to produce
// byte-identical evidence renderings.
//
// The rendering is the strong form of the claim: it covers the page plan, every
// page's shape, the window, the refusal codes and messages, the qid census, the
// cursor counts and the frame deltas at once. What it deliberately excludes is any
// WAL BYTE total that could be non-zero — see [BoltStreamEvidence.String] for why a
// committed write's byte count is a function of process history rather than of the
// seed.
func TestBoltStreamSemantics_Determinism(t *testing.T) {
	defer goleak.VerifyNone(t)
	const seed = 0x2484_D371
	ctx, cancel := context.WithTimeout(context.Background(), 2*boltStreamArmTimeout)
	defer cancel()

	first, err := RunBoltStreamSemantics(ctx, seed)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := RunBoltStreamSemantics(ctx, seed)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if a, b := first.String(), second.String(); a != b {
		t.Fatalf("two runs of seed %#x rendered differently:\n--- first ---\n%s\n--- second ---\n%s", seed, a, b)
	}
	// The rendering summarises the rows; compare them element by element too, so a
	// change in the record VALUES that the summary happens not to show cannot pass.
	if v := boltStreamCompareRows("determinism", "the second run's reference drain",
		second.Reference, first.Reference); len(v) > 0 {
		t.Fatalf("the reference drain is not a function of the seed: %v", v)
	}
	if v := boltStreamCompareRows("determinism", "the second run's paged drain",
		second.Paged, first.Paged); len(v) > 0 {
		t.Fatalf("the paged drain is not a function of the seed: %v", v)
	}
}

// -----------------------------------------------------------------------------
// Live controls: real servers, not doctored structs
// -----------------------------------------------------------------------------

// TestBoltStreamSemantics_QIDIsNotRoutable is the REFUTATION, driven live.
//
// The task this scenario implements asked for "QID multiplexing" and "QID
// routing". This server has neither: it keeps exactly one open stream per session,
// never names it, and refuses any attempt to address a stream by a positive qid.
// The refutation is asserted here against a real server rather than argued from the
// source, on all three of its limbs at once.
func TestBoltStreamSemantics_QIDIsNotRoutable(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	dial := func() *WireClient {
		t.Helper()
		c, err := srv.Dial()
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if err := c.Connect(ctx); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		return c
	}

	// Limb 1: RUN never mints a positive qid, so a client has none to send back.
	c := dial()
	resp, err := c.Run("UNWIND range(1,5) AS x RETURN x", nil)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	succ, ok := resp.(*proto.Success)
	if !ok {
		t.Fatalf("RUN reply %T, want *proto.Success", resp)
	}
	if q, ok := succ.Metadata["qid"].(int64); !ok || q != -1 {
		t.Fatalf("RUN reported qid %v (present=%v), want -1", succ.Metadata["qid"], ok)
	}

	// Limb 2: a positive qid is refused on PULL and on DISCARD, with the same code
	// and the same message shape, and delivers no row.
	const wantCode = streamCodeRequestInvalid
	for _, probe := range []struct {
		name string
		send func(*WireClient, int64) ([]*proto.Record, any, error)
	}{
		{"PULL", func(c *WireClient, q int64) ([]*proto.Record, any, error) { return c.PullQID(-1, q) }},
		{"DISCARD", func(c *WireClient, q int64) ([]*proto.Record, any, error) { return c.DiscardQID(-1, q) }},
	} {
		t.Run(probe.name, func(t *testing.T) {
			cc := dial()
			if _, err := cc.Run("UNWIND range(1,5) AS x RETURN x", nil); err != nil {
				t.Fatalf("RUN: %v", err)
			}
			const qid = 42
			recs, terminal, err := probe.send(cc, qid)
			if err != nil {
				t.Fatalf("%s qid=%d: %v", probe.name, qid, err)
			}
			if len(recs) != 0 {
				t.Errorf("%s with qid=%d delivered %d RECORD(s), want 0", probe.name, qid, len(recs))
			}
			f, ok := terminal.(*proto.Failure)
			if !ok {
				t.Fatalf("%s with qid=%d drew %T, want *proto.Failure", probe.name, qid, terminal)
			}
			if f.Code != wantCode {
				t.Errorf("code = %q, want %q", f.Code, wantCode)
			}
			if want := "no such query: qid 42"; f.Message != want {
				t.Errorf("message = %q, want %q", f.Message, want)
			}
			// The control: the SAME message with qid=-1 is served.
			cc2 := dial()
			if _, err := cc2.Run("UNWIND range(1,5) AS x RETURN x", nil); err != nil {
				t.Fatalf("control RUN: %v", err)
			}
			ctlRecs, ctlTerm, err := cc2.PullQID(-1, -1)
			if err != nil {
				t.Fatalf("control PULL qid=-1: %v", err)
			}
			if !isSuccess(ctlTerm) || len(ctlRecs) != 5 {
				t.Fatalf("control with qid=-1 drew %T with %d row(s), want SUCCESS with 5", ctlTerm, len(ctlRecs))
			}
		})
	}

	// Limb 3: a second RUN while a stream is open is refused by the STATE machine,
	// and the refusal names the ORIGIN state. The needle is the whole "in state X"
	// phrase: "TX_STREAMING" contains "STREAMING", so a bare containment check would
	// not tell the two gates apart.
	for _, probe := range []struct {
		name   string
		openTx bool
		want   string
	}{
		{"streaming", false, "illegal message *proto.Run in state STREAMING"},
		{"tx_streaming", true, "illegal message *proto.Run in state TX_STREAMING"},
	} {
		t.Run("second-run-"+probe.name, func(t *testing.T) {
			cc := dial()
			if probe.openTx {
				if r, err := cc.Begin(); err != nil {
					t.Fatalf("BEGIN: %v", err)
				} else if !isSuccess(r) {
					t.Fatalf("BEGIN drew %T", r)
				}
			}
			if _, err := cc.Run("UNWIND range(1,5) AS x RETURN x", nil); err != nil {
				t.Fatalf("first RUN: %v", err)
			}
			second, err := cc.Run("RETURN 1", nil)
			if err != nil {
				t.Fatalf("second RUN: %v", err)
			}
			f, ok := second.(*proto.Failure)
			if !ok {
				t.Fatalf("second RUN drew %T, want *proto.Failure — a second open stream is not "+
					"reachable on this server", second)
			}
			if f.Code != wantCode {
				t.Errorf("code = %q, want %q", f.Code, wantCode)
			}
			if f.Message != probe.want {
				t.Errorf("message = %q, want %q", f.Message, probe.want)
			}
		})
	}
}

// TestBoltStreamSemantics_InFlightCapIsTheOnlyDifference is the live control for
// the cursor-accumulation arm: the SAME script, with only Options.MaxInFlightPerConnection
// changed, must stop being refused.
//
// This is a real alternative CONFIGURATION rather than a doctored evidence value.
// It pins the refusal on the cap and not on explicit transactions, on sequential
// RUNs, on the CREATE statement, or on a typo in the harness — every one of which
// would refuse the raised-cap run too.
func TestBoltStreamSemantics_InFlightCapIsTheOnlyDifference(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	// The script: BEGIN, then boltStreamInFlightCap+1 RUN+PULL cycles. It returns the
	// reply to the cycle that would exceed a cap of boltStreamInFlightCap.
	script := func(t *testing.T, cap int) any {
		t.Helper()
		srv, err := NewSimServerInFlight(SimEngineForServer(), clock.Real(), cap)
		if err != nil {
			t.Fatalf("NewSimServerInFlight(%d): %v", cap, err)
		}
		defer func() { _ = srv.Close() }()
		c, err := srv.Dial()
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer func() { _ = c.Close() }()
		if err := c.Connect(ctx); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		if r, err := c.Begin(); err != nil {
			t.Fatalf("BEGIN: %v", err)
		} else if !isSuccess(r) {
			t.Fatalf("BEGIN drew %T", r)
		}
		var last any
		for i := 0; i <= boltStreamInFlightCap; i++ {
			last, err = c.Run("CREATE (:CapControl) RETURN 1 AS one", nil)
			if err != nil {
				t.Fatalf("cycle %d RUN: %v", i, err)
			}
			if !isSuccess(last) {
				return last
			}
			if _, term, err := c.PullAll(); err != nil {
				t.Fatalf("cycle %d PULL: %v", i, err)
			} else if !isSuccess(term) {
				return term
			}
		}
		return last
	}

	// At the scenario's cap, the last cycle is refused with the typed error.
	atCap := script(t, boltStreamInFlightCap)
	f, ok := atCap.(*proto.Failure)
	if !ok {
		t.Fatalf("at cap=%d the over-cap cycle drew %T, want *proto.Failure", boltStreamInFlightCap, atCap)
	}
	if f.Code != streamCodeLimitExceeded {
		t.Errorf("code = %q, want %q", f.Code, streamCodeLimitExceeded)
	}
	if want := "cap=3"; !strings.Contains(f.Message, want) {
		t.Errorf("message %q does not name %q", f.Message, want)
	}
	if got := boltStreamParseOpen(f.Message); got != boltStreamInFlightCap {
		t.Errorf("the refusal reported open=%d, want %d", got, boltStreamInFlightCap)
	}

	// With the cap raised and NOTHING else changed, the identical script completes.
	raised := script(t, boltStreamInFlightCap+8)
	if !isSuccess(raised) {
		t.Fatalf("with the cap raised to %d the identical script still drew %T (%+v): the refusal was "+
			"not caused by the cap", boltStreamInFlightCap+8, raised, raised)
	}
}

// TestBoltStreamSemantics_IgnoredIsNotAnAcknowledgement guards the rule the whole
// battery rests on: only the TERMINAL reply acknowledges, and an IGNORED is a
// refusal.
//
// A qid refusal routes through enterFailed, and a FAILED session soft-ignores the
// next request-phase message. If [boltStreamRunner.statementAcked] treated a
// non-FAILURE as success, every "the session is still usable" clause in this file
// would pass on a poisoned connection.
func TestBoltStreamSemantics_IgnoredIsNotAnAcknowledgement(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()
	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	r := &boltStreamRunner{srv: srv, ev: &BoltStreamEvidence{}, rng: NewSeed(1)}

	// Healthy connection: the terminal reply acknowledges.
	if acked, err := r.statementAcked(c); err != nil {
		t.Fatalf("statementAcked on a healthy connection: %v", err)
	} else if !acked {
		t.Fatal("statementAcked reported false on a healthy connection")
	}

	// Poison it with a positive qid, then confirm the reply really is IGNORED and
	// that statementAcked refuses to call it an acknowledgement.
	if _, err := c.Run("UNWIND range(1,5) AS x RETURN x", nil); err != nil {
		t.Fatalf("RUN: %v", err)
	}
	if _, terminal, err := c.PullQID(-1, 9); err != nil {
		t.Fatalf("PULL qid=9: %v", err)
	} else if failureCode(terminal) != streamCodeRequestInvalid {
		t.Fatalf("PULL qid=9 drew %T, want a Request.Invalid FAILURE", terminal)
	}
	next, err := c.Run("RETURN 1 AS one", nil)
	if err != nil {
		t.Fatalf("post-refusal RUN: %v", err)
	}
	if !isIgnored(next) {
		t.Fatalf("post-refusal RUN drew %T, want *proto.Ignored", next)
	}
	if acked, err := r.statementAcked(c); err != nil {
		t.Fatalf("statementAcked on a poisoned connection: %v", err)
	} else if acked {
		t.Fatal("statementAcked accepted an IGNORED as an acknowledgement: every \"the session is " +
			"still usable\" clause would then pass on a poisoned connection")
	}
}

// -----------------------------------------------------------------------------
// Falsifiability: one perturbed field per clause
// -----------------------------------------------------------------------------

// boltStreamSyntheticRows builds the reference query's row shape for x in
// [from, to], matching what the server actually encodes: an Integer, a String, a
// Boolean, a List of two Integers, and a Float. The shapes were MEASURED off the
// wire rather than assumed (see [TestBoltStreamSemantics_Measurements], which pins
// the row width, and the arms test, which compares these against a real drain
// through the same comparator).
func boltStreamSyntheticRows(from, to int) [][]packstream.Value {
	out := make([][]packstream.Value, 0, to-from+1)
	for x := from; x <= to; x++ {
		out = append(out, []packstream.Value{
			int64(x),
			strconv.Itoa(x),
			x%3 == 0,
			[]packstream.Value{int64(x), int64(x + 1)},
			float64(x) / 2,
		})
	}
	return out
}

// healthyBoltStreamEvidence is the baseline the falsifiability tables perturb: a
// run in which every clause of both adjudicators is satisfied.
//
// The page plan is the WORST case the draw can produce — every page at
// boltStreamMaxPage — so the page count is the arithmetic minimum and the final
// page is short. That keeps the baseline's shape independent of any particular
// seed.
func healthyBoltStreamEvidence() *BoltStreamEvidence {
	const page = boltStreamMaxPage
	ref := boltStreamSyntheticRows(1, boltStreamRows)

	var sizes []int64
	var pages []BoltStreamPage
	delivered := 0
	for delivered < boltStreamRows {
		got := min(page, boltStreamRows-delivered)
		delivered += got
		sizes = append(sizes, page)
		pages = append(pages, BoltStreamPage{
			Requested: page,
			Delivered: got,
			HasMore:   delivered < boltStreamRows,
			// The bookmark rides on the terminal reply only.
			Bookmarked: delivered >= boltStreamRows,
			Terminal:   "SUCCESS",
		})
	}

	// The window: 21 rows paged over two pages, then 7 rows discarded, then the rest.
	const prefix, discard = 21, 7
	runQIDs := make([]int64, 26)
	for i := range runQIDs {
		runQIDs[i] = -1
	}

	return &BoltStreamEvidence{
		Seed:              1,
		Reference:         ref,
		ReferenceTerminal: "SUCCESS",
		ReferenceHasMore:  false,
		PageSizes:         sizes,
		Pages:             pages,
		Paged:             boltStreamSyntheticRows(1, boltStreamRows),
		PagingReusable:    true,

		WindowPageSizes:   []int64{16, 5},
		WindowPrefixPages: 2,
		WindowPrefixPagesSeen: []BoltStreamPage{
			{Requested: 16, Delivered: 16, HasMore: true, Terminal: "SUCCESS"},
			{Requested: 5, Delivered: 5, HasMore: true, Terminal: "SUCCESS"},
		},
		WindowPrefix:   boltStreamSyntheticRows(1, prefix),
		WindowDiscardN: discard,
		WindowDiscard: BoltStreamPage{
			Requested: discard, Delivered: 0, HasMore: true, Terminal: "SUCCESS", Discard: true,
		},
		WindowSuffix: boltStreamSyntheticRows(prefix+discard+1, boltStreamRows),
		WindowSuffixPage: BoltStreamPage{
			Requested: -1, Delivered: boltStreamRows - prefix - discard,
			HasMore: false, Bookmarked: true, Terminal: "SUCCESS",
		},
		WindowReusable: true,

		EffectDiscard: BoltStreamPage{
			Requested: -1, Delivered: 0, HasMore: false, Bookmarked: true, Terminal: "SUCCESS", Discard: true,
		},
		EffectStats: map[string]int64{
			"contains-updates": 1, "labels-added": 1, "nodes-created": 1, "properties-set": 1,
		},
		EffectLive: 1, EffectRecovered: 1, EffectReusable: true,

		Refusals: []BoltStreamRefusal{
			{
				Name: "pull-positive-qid", WantCode: streamCodeRequestInvalid,
				GotCode:     streamCodeRequestInvalid,
				WantMessage: "no such query: qid 733", GotMessage: "no such query: qid 733",
				NextTerminal: "IGNORED", PriorAck: true, RecoveredAfterReset: true,
			},
			{
				Name: "discard-positive-qid", WantCode: streamCodeRequestInvalid,
				GotCode:     streamCodeRequestInvalid,
				WantMessage: "no such query: qid 700", GotMessage: "no such query: qid 700",
				NextTerminal: "IGNORED", PriorAck: true, RecoveredAfterReset: true,
			},
			{
				Name: "second-run-in-streaming", WantCode: streamCodeRequestInvalid,
				GotCode:            streamCodeRequestInvalid,
				WantMessage:        "illegal message *proto.Run in state STREAMING",
				GotMessage:         "illegal message *proto.Run in state STREAMING",
				WantStateInMessage: "STREAMING",
				NextTerminal:       "IGNORED", PriorAck: true, RecoveredAfterReset: true,
			},
			{
				Name: "second-run-in-tx-streaming", WantCode: streamCodeRequestInvalid,
				GotCode:            streamCodeRequestInvalid,
				WantMessage:        "illegal message *proto.Run in state TX_STREAMING",
				GotMessage:         "illegal message *proto.Run in state TX_STREAMING",
				WantStateInMessage: "TX_STREAMING",
				NextTerminal:       "IGNORED", PriorAck: true, RecoveredAfterReset: true,
			},
		},
		ProbedQIDs:     []int64{733, 700},
		RunQIDs:        runQIDs,
		QIDControlRows: boltStreamRows,

		TxArms: []BoltStreamTxArm{
			{
				Name: "cursors-under-cap", Cycles: boltStreamInFlightCap, Committed: true,
				CursorsAtRefusal: -1,
				FramesBefore:     0, FramesAfter: 10, BytesBefore: 0, BytesAfter: 500,
			},
			{
				Name: "cursors-over-cap", Cycles: boltStreamInFlightCap,
				CursorsAtRefusal: boltStreamInFlightCap, RefusalObserved: true,
				RefusalCode: streamCodeLimitExceeded,
				RefusalMessage: "bolt: per-connection in-flight cursor cap reached (cap=3, open=3); " +
					"commit/rollback or pull/discard before issuing more queries",
				FramesBefore: 10, FramesAfter: 10, BytesBefore: 500, BytesAfter: 500,
			},
		},
		CommittedLive: boltStreamInFlightCap, CommittedRecovered: boltStreamInFlightCap,
		DoomedLive: 0, DoomedRecovered: 0,
	}
}

// healthyBoltStreamStallEvidence is the baseline for the concurrent arm.
func healthyBoltStreamStallEvidence() *BoltStreamEvidence {
	values := make([]int64, boltStreamStallSurfaceRows)
	for i := range values {
		values[i] = int64(i + 1)
	}
	var pages []BoltStreamPage
	delivered := 0
	for delivered < boltStreamStallSurfaceRows {
		got := min(boltStreamStallSurfacePage, boltStreamStallSurfaceRows-delivered)
		delivered += got
		pages = append(pages, BoltStreamPage{
			Requested: boltStreamStallSurfacePage,
			Delivered: got,
			HasMore:   delivered < boltStreamStallSurfaceRows,
			Terminal:  "SUCCESS",
		})
	}
	return &BoltStreamEvidence{
		Seed:               2,
		StallArm:           true,
		StallRecordsPulled: 3,
		StallBufferedPeak:  simConnBufferSize,
		StallParked:        true,
		StallClosedCleanly: true,
		StallSurfaceValues: values,
		StallSurfacePages:  pages,
		RunQIDs:            []int64{-1},
	}
}

// TestBoltStreamSemantics_HealthyEvidencePasses is the tables' own precondition: the
// baselines must satisfy both adjudicators, or a perturbation proves nothing about
// the clause it aimed at.
func TestBoltStreamSemantics_HealthyEvidencePasses(t *testing.T) {
	defer goleak.VerifyNone(t)
	for name, ev := range map[string]*BoltStreamEvidence{
		"deterministic": healthyBoltStreamEvidence(),
		"stall":         healthyBoltStreamStallEvidence(),
	} {
		if v := checkBoltStreamSemantics(ev); len(v) > 0 {
			t.Fatalf("the hand-built healthy %s evidence fails the contract: %v", name, v)
		}
		if v := checkBoltStreamSemanticsNonVacuity(ev); len(v) > 0 {
			t.Fatalf("the hand-built healthy %s evidence fails the coverage gate: %v", name, v)
		}
	}
}

// TestBoltStreamSemantics_ContractFalsifiability perturbs one field per sub-test and
// requires the named clause to fire. A perturbation may make other clauses fire too;
// what the table asserts is that the named one is not silent.
func TestBoltStreamSemantics_ContractFalsifiability(t *testing.T) {
	defer goleak.VerifyNone(t)
	tests := []struct {
		name    string
		clause  string
		perturb func(*BoltStreamEvidence)
	}{
		{"the reference pull-all did not exhaust the stream", "reference-drain", func(e *BoltStreamEvidence) {
			e.ReferenceHasMore = true
		}},
		{"the reference drain failed", "reference-drain", func(e *BoltStreamEvidence) {
			e.ReferenceTerminal = "FAILURE"
		}},
		{"the drain took fewer pages than it planned", "paging-page-count", func(e *BoltStreamEvidence) {
			e.PageSizes = append(e.PageSizes, boltStreamMaxPage)
		}},
		{"a row went missing from the middle of the paged drain", "paging-equivalence", func(e *BoltStreamEvidence) {
			e.Paged = append(e.Paged[:40:40], e.Paged[41:]...)
		}},
		{"two rows arrived out of order", "paging-equivalence", func(e *BoltStreamEvidence) {
			e.Paged[40], e.Paged[41] = e.Paged[41], e.Paged[40]
		}},
		{"an Integer arrived as a Float with identical text", "paging-equivalence", func(e *BoltStreamEvidence) {
			// The point of comparing DECODED values rather than String() renderings:
			// fmt of int64(41) and of float64(41) are the same three characters, so a
			// text comparison would pass here.
			e.Paged[40] = []packstream.Value{
				float64(41), e.Paged[40][1], e.Paged[40][2], e.Paged[40][3], e.Paged[40][4],
			}
		}},
		{"a nested List lost an element", "paging-equivalence", func(e *BoltStreamEvidence) {
			e.Paged[40] = []packstream.Value{
				e.Paged[40][0], e.Paged[40][1], e.Paged[40][2],
				[]packstream.Value{int64(41)}, e.Paged[40][4],
			}
		}},
		{"a page delivered more rows than it asked for", "paging-page-size", func(e *BoltStreamEvidence) {
			e.Pages[0].Delivered = boltStreamMaxPage + 1
		}},
		{"has_more was false on a non-final page", "paging-has_more", func(e *BoltStreamEvidence) {
			e.Pages[0].HasMore = false
		}},
		{"has_more was true on the final page", "paging-has_more", func(e *BoltStreamEvidence) {
			e.Pages[len(e.Pages)-1].HasMore = true
		}},
		{"a page ended in FAILURE", "paging-terminal", func(e *BoltStreamEvidence) {
			e.Pages[1].Terminal = "FAILURE"
			e.Pages[1].Code = "Neo.DatabaseError.General.UnknownError"
		}},
		{"a non-final page carried the bookmark", "paging-bookmark", func(e *BoltStreamEvidence) {
			e.Pages[0].Bookmarked = true
		}},
		{"the terminal page carried no bookmark", "paging-bookmark", func(e *BoltStreamEvidence) {
			e.Pages[len(e.Pages)-1].Bookmarked = false
		}},
		{"the session was unusable after a full drain", "paging-reusable", func(e *BoltStreamEvidence) {
			e.PagingReusable = false
		}},

		{"the discard window ran past the reference", "window-bounds", func(e *BoltStreamEvidence) {
			e.WindowDiscardN = boltStreamRows
		}},
		{"DISCARD dropped one row too many", "window-equivalence", func(e *BoltStreamEvidence) {
			e.WindowSuffix = e.WindowSuffix[1:]
		}},
		{"DISCARD dropped one row too few", "window-equivalence", func(e *BoltStreamEvidence) {
			e.WindowSuffix = append(boltStreamSyntheticRows(28, 28), e.WindowSuffix...)
		}},
		{"the mid-stream DISCARD delivered a row", "window-no-delivery", func(e *BoltStreamEvidence) {
			e.WindowDiscard.Delivered = 1
		}},
		{"the mid-stream DISCARD reported has_more=false", "window-has_more", func(e *BoltStreamEvidence) {
			e.WindowDiscard.HasMore = false
		}},
		{"the mid-stream DISCARD failed", "window-terminal", func(e *BoltStreamEvidence) {
			e.WindowDiscard.Terminal = "FAILURE"
		}},
		{"a non-terminal DISCARD carried a bookmark", "window-bookmark", func(e *BoltStreamEvidence) {
			e.WindowDiscard.Bookmarked = true
		}},
		{"the closing pull-all failed", "window-suffix-terminal", func(e *BoltStreamEvidence) {
			e.WindowSuffixPage.Terminal = "IGNORED"
		}},
		{"the closing pull-all left rows behind", "window-suffix-has_more", func(e *BoltStreamEvidence) {
			e.WindowSuffixPage.HasMore = true
		}},
		{"the closing pull-all carried no bookmark", "window-suffix-bookmark", func(e *BoltStreamEvidence) {
			e.WindowSuffixPage.Bookmarked = false
		}},
		{"the session was unusable after a mid-stream DISCARD", "window-reusable", func(e *BoltStreamEvidence) {
			e.WindowReusable = false
		}},

		{"the discarded write's rows were delivered anyway", "effect-no-delivery", func(e *BoltStreamEvidence) {
			e.EffectDiscard.Delivered = 1
		}},
		{"the discarded write's terminal reply was not terminal", "effect-terminal", func(e *BoltStreamEvidence) {
			e.EffectDiscard.Bookmarked = false
		}},
		{"the discarded write reported no counters", "effect-stats", func(e *BoltStreamEvidence) {
			delete(e.EffectStats, "nodes-created")
		}},
		{"the discarded write was not applied", "effect-live", func(e *BoltStreamEvidence) {
			e.EffectLive = 0
		}},
		{"the discarded write did not survive recovery", "effect-recovered", func(e *BoltStreamEvidence) {
			e.EffectRecovered = 0
		}},
		{"the session was unusable after discarding a write", "effect-reusable", func(e *BoltStreamEvidence) {
			e.EffectReusable = false
		}},

		{"a positive qid was ADMITTED", "refusal-admitted", func(e *BoltStreamEvidence) {
			e.Refusals[0].Accepted = true
		}},
		{"a qid refusal carried the wrong code", "refusal-code", func(e *BoltStreamEvidence) {
			e.Refusals[0].GotCode = "Neo.ClientError.Security.Unauthorized"
		}},
		{"a qid refusal carried the wrong message", "refusal-message", func(e *BoltStreamEvidence) {
			e.Refusals[0].GotMessage = "no such query: qid 734"
		}},
		{"a STREAMING refusal named TX_STREAMING instead", "refusal-origin-state", func(e *BoltStreamEvidence) {
			// The near-miss the whole-phrase needle defends against: "TX_STREAMING"
			// contains "STREAMING", so a bare containment check would pass this.
			e.Refusals[2].GotMessage = "illegal message *proto.Run in state TX_STREAMING"
		}},
		{"a refusal named FAILED, so the state gate refused first", "refusal-origin-state", func(e *BoltStreamEvidence) {
			e.Refusals[3].GotMessage = "illegal message *proto.Run in state FAILED"
		}},
		{"a refused message still delivered a row", "refusal-delivery", func(e *BoltStreamEvidence) {
			e.Refusals[1].Delivered = 2
		}},
		{"the next request after a refusal was SERVED", "refusal-poisons-session", func(e *BoltStreamEvidence) {
			e.Refusals[0].NextTerminal = "SUCCESS"
		}},
		{"the connection was dead after a refusal", "refusal-poisons-session", func(e *BoltStreamEvidence) {
			e.Refusals[0].NextTerminal = "TRANSPORT"
		}},
		{"RESET did not restore the session", "refusal-reset-recovers", func(e *BoltStreamEvidence) {
			e.Refusals[1].RecoveredAfterReset = false
		}},

		{"a RUN minted a positive qid", "run-qid-current", func(e *BoltStreamEvidence) {
			e.RunQIDs[3] = 0
		}},
		{"the qid=-1 control was short-changed", "qid-control", func(e *BoltStreamEvidence) {
			e.QIDControlRows = boltStreamRows - 1
		}},

		{"an in-flight arm did not run", "inflight-roster", func(e *BoltStreamEvidence) {
			e.TxArms = e.TxArms[:1]
		}},
		{"the under-cap transaction was refused early", "inflight-accumulates", func(e *BoltStreamEvidence) {
			e.TxArms[0].Cycles = 1
		}},
		{"the under-cap COMMIT was not acknowledged", "inflight-commit", func(e *BoltStreamEvidence) {
			e.TxArms[0].Committed = false
		}},
		{"the committed transaction reached no WAL frame", "inflight-admit-frames", func(e *BoltStreamEvidence) {
			e.TxArms[0].FramesAfter = e.TxArms[0].FramesBefore
		}},
		{"a committed write is missing from the live engine", "inflight-committed-live", func(e *BoltStreamEvidence) {
			e.CommittedLive = boltStreamInFlightCap - 1
		}},
		{"a committed write did not survive recovery", "inflight-committed-recovered", func(e *BoltStreamEvidence) {
			e.CommittedRecovered = 0
		}},
		{"the cap refused the first RUN instead of accumulating", "inflight-over-cycles", func(e *BoltStreamEvidence) {
			e.TxArms[1].Cycles = 0
		}},
		{"the over-cap RUN drew no typed error", "inflight-typed", func(e *BoltStreamEvidence) {
			e.TxArms[1].RefusalObserved = false
		}},
		{"the cap breach carried the wrong code", "inflight-cap-code", func(e *BoltStreamEvidence) {
			e.TxArms[1].RefusalCode = streamCodeRequestInvalid
		}},
		{"the cap breach did not name the bound", "inflight-cap-named", func(e *BoltStreamEvidence) {
			e.TxArms[1].RefusalMessage = "bolt: too many queries"
		}},
		{"the server's own cursor count disagreed", "inflight-open-count", func(e *BoltStreamEvidence) {
			e.TxArms[1].CursorsAtRefusal = boltStreamInFlightCap + 1
		}},
		{"the doomed transaction appended a WAL frame", "inflight-doomed-frames", func(e *BoltStreamEvidence) {
			e.TxArms[1].FramesAfter++
		}},
		{"the doomed transaction appended WAL bytes", "inflight-doomed-bytes", func(e *BoltStreamEvidence) {
			e.TxArms[1].BytesAfter += 183
		}},
		{"a doomed write is visible in the live engine", "inflight-doomed-live", func(e *BoltStreamEvidence) {
			e.DoomedLive = 1
		}},
		{"a doomed write survived recovery", "inflight-doomed-recovered", func(e *BoltStreamEvidence) {
			e.DoomedRecovered = 1
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := healthyBoltStreamEvidence()
			tc.perturb(ev)
			v := checkBoltStreamSemantics(ev)
			if !violationsMentionClause(v, tc.clause) {
				t.Fatalf("perturbation %q did not fire clause %q; violations: %v", tc.name, tc.clause, v)
			}
		})
	}
}

// TestBoltStreamSemantics_NonVacuityFalsifiability does the same for the coverage
// gate: every clause must fire when the run stops exercising what it claims to.
func TestBoltStreamSemantics_NonVacuityFalsifiability(t *testing.T) {
	defer goleak.VerifyNone(t)
	tests := []struct {
		name    string
		clause  string
		perturb func(*BoltStreamEvidence)
	}{
		{"the reference drain was empty", "nonvacuity-reference", func(e *BoltStreamEvidence) {
			e.Reference = nil
		}},
		{"the drain took a single page", "nonvacuity-pages", func(e *BoltStreamEvidence) {
			e.Pages = e.Pages[:1]
		}},
		{"has_more was never true", "nonvacuity-has_more", func(e *BoltStreamEvidence) {
			for i := range e.Pages {
				e.Pages[i].HasMore = false
			}
		}},
		{"has_more was never false", "nonvacuity-has_more", func(e *BoltStreamEvidence) {
			for i := range e.Pages {
				e.Pages[i].HasMore = true
			}
		}},
		{"no page was ever bounded by its n", "nonvacuity-bounded-page", func(e *BoltStreamEvidence) {
			for i := range e.Pages {
				e.Pages[i].Delivered = 1
			}
		}},
		{"the discard window started at row zero", "nonvacuity-window", func(e *BoltStreamEvidence) {
			e.WindowPrefix = nil
		}},
		{"the discard window ran to the end of the stream", "nonvacuity-window", func(e *BoltStreamEvidence) {
			e.WindowDiscardN = int64(boltStreamRows - len(e.WindowPrefix))
		}},
		{"nothing was discarded at all", "nonvacuity-window", func(e *BoltStreamEvidence) {
			e.WindowDiscardN = 0
		}},
		{"the discarded statement had no effect to confirm", "nonvacuity-effect", func(e *BoltStreamEvidence) {
			e.EffectStats = nil
		}},
		{"no transaction was seen reaching the WAL", "nonvacuity-wal-instrument", func(e *BoltStreamEvidence) {
			e.TxArms[0].FramesAfter = e.TxArms[0].FramesBefore
		}},
		{"a refusal probe did not run", "nonvacuity-refusal-roster", func(e *BoltStreamEvidence) {
			e.Refusals = e.Refusals[:3]
		}},
		{"a probe's connection was never shown to work", "nonvacuity-prior-ack", func(e *BoltStreamEvidence) {
			e.Refusals[0].PriorAck = false
		}},
		{"a qid probe used the current-stream qid", "nonvacuity-qid-positive", func(e *BoltStreamEvidence) {
			e.ProbedQIDs[1] = -1
		}},
		{"no explicit qid was sent", "nonvacuity-qid-positive", func(e *BoltStreamEvidence) {
			e.ProbedQIDs = nil
		}},
		{"the qid control delivered nothing", "nonvacuity-qid-control", func(e *BoltStreamEvidence) {
			e.QIDControlRows = 0
		}},
		{"the qid census inspected almost nothing", "nonvacuity-run-qids", func(e *BoltStreamEvidence) {
			e.RunQIDs = e.RunQIDs[:boltStreamMinRunQIDs-1]
		}},
		{"no probe recovered after RESET", "nonvacuity-reset-recovery", func(e *BoltStreamEvidence) {
			for i := range e.Refusals {
				e.Refusals[i].RecoveredAfterReset = false
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := healthyBoltStreamEvidence()
			tc.perturb(ev)
			v := checkBoltStreamSemanticsNonVacuity(ev)
			if !violationsMentionClause(v, tc.clause) {
				t.Fatalf("perturbation %q did not fire clause %q; shortfalls: %v", tc.name, tc.clause, v)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// The concurrent stall arm
// -----------------------------------------------------------------------------

// TestBoltStreamStall_Arm drives the concurrent slow-consumer arm for real at
// several seeds.
//
// It asserts only what every interleaving must share. How many records reach the
// bounded buffer before the consumer stops is a scheduling outcome, so the
// distribution is REPORTED and the oracle is the invariant: the writer parked, the
// buffer stayed bounded, and a fresh connection's paging still matched arithmetic.
func TestBoltStreamStall_Arm(t *testing.T) {
	defer goleak.VerifyNone(t)
	for _, seed := range boltStreamTestSeeds[:3] {
		ctx, cancel := context.WithTimeout(context.Background(), boltStreamArmTimeout)
		ev, err := RunBoltStreamStall(ctx, seed)
		cancel()
		if err != nil {
			t.Fatalf("seed %#x: %v", seed, err)
		}
		if v := checkBoltStreamSemantics(ev); len(v) > 0 {
			t.Fatalf("seed %#x contract violations %v\n%s", seed, v, ev)
		}
		if v := checkBoltStreamSemanticsNonVacuity(ev); len(v) > 0 {
			t.Fatalf("seed %#x coverage shortfall %v\n%s", seed, v, ev)
		}
		t.Logf("seed %#x: drained %d/%d row(s), queue peaked at %d of %d byte(s), post-stall drain %d row(s) over %d page(s)",
			seed, ev.StallRecordsPulled, slowConsumerResultRows, ev.StallBufferedPeak, simConnBufferSize,
			len(ev.StallSurfaceValues), len(ev.StallSurfacePages))
	}
}

// TestBoltStreamStall_Falsifiability perturbs one field per sub-test of the
// concurrent arm's baseline.
func TestBoltStreamStall_Falsifiability(t *testing.T) {
	defer goleak.VerifyNone(t)
	tests := []struct {
		name    string
		clause  string
		perturb func(*BoltStreamEvidence)
	}{
		{"the stream ended before the consumer could stall", "stall-parked", func(e *BoltStreamEvidence) {
			e.StallParked = false
		}},
		{"the connection buffer broke its own bound", "stall-bounded", func(e *BoltStreamEvidence) {
			e.StallBufferedPeak = simConnBufferSize + 1
		}},
		{"the consumer drained nothing", "stall-prefix", func(e *BoltStreamEvidence) {
			e.StallRecordsPulled = 0
		}},
		{"the consumer drained the whole result", "stall-prefix", func(e *BoltStreamEvidence) {
			e.StallRecordsPulled = slowConsumerResultRows
		}},
		{"the teardown reported a transport fault", "stall-clean-close", func(e *BoltStreamEvidence) {
			e.StallClosedCleanly = false
		}},
		{"the post-stall drain lost a row", "stall-surface-rows", func(e *BoltStreamEvidence) {
			e.StallSurfaceValues = e.StallSurfaceValues[1:]
		}},
		{"the post-stall drain reordered two rows", "stall-surface-order", func(e *BoltStreamEvidence) {
			e.StallSurfaceValues[9], e.StallSurfaceValues[10] = e.StallSurfaceValues[10], e.StallSurfaceValues[9]
		}},
		{"a post-stall row arrived with the wrong shape", "stall-surface-order", func(e *BoltStreamEvidence) {
			e.StallSurfaceValues[4] = -1
		}},
		{"a post-stall page failed", "stall-surface-terminal", func(e *BoltStreamEvidence) {
			e.StallSurfacePages[1].Terminal = "FAILURE"
		}},
		{"has_more was wrong on a post-stall page", "stall-surface-has_more", func(e *BoltStreamEvidence) {
			e.StallSurfacePages[0].HasMore = false
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := healthyBoltStreamStallEvidence()
			tc.perturb(ev)
			v := checkBoltStreamSemantics(ev)
			if !violationsMentionClause(v, tc.clause) {
				t.Fatalf("perturbation %q did not fire clause %q; violations: %v", tc.name, tc.clause, v)
			}
		})
	}
}

// TestBoltStreamStall_NonVacuityFalsifiability does the same for the stall arm's
// coverage gate.
func TestBoltStreamStall_NonVacuityFalsifiability(t *testing.T) {
	defer goleak.VerifyNone(t)
	tests := []struct {
		name    string
		clause  string
		perturb func(*BoltStreamEvidence)
	}{
		{"the stream was never opened", "nonvacuity-stall-open", func(e *BoltStreamEvidence) {
			e.StallRecordsPulled = 0
		}},
		{"the writer was never observed parked", "nonvacuity-stall-parked", func(e *BoltStreamEvidence) {
			e.StallBufferedPeak = 0
		}},
		{"the post-stall drain was a pull-all", "nonvacuity-stall-pages", func(e *BoltStreamEvidence) {
			e.StallSurfacePages = e.StallSurfacePages[:1]
		}},
		{"no RUN reply was inspected", "nonvacuity-stall-run-qid", func(e *BoltStreamEvidence) {
			e.RunQIDs = nil
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := healthyBoltStreamStallEvidence()
			tc.perturb(ev)
			v := checkBoltStreamSemanticsNonVacuity(ev)
			if !violationsMentionClause(v, tc.clause) {
				t.Fatalf("perturbation %q did not fire clause %q; shortfalls: %v", tc.name, tc.clause, v)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Wiring and the sweep
// -----------------------------------------------------------------------------

// TestBoltStreamSemantics_Scenario drives both scenarios through their registered
// entry points and requires a nil report — the shape the CLI and the swarm read.
func TestBoltStreamSemantics_Scenario(t *testing.T) {
	defer goleak.VerifyNone(t)
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	for _, name := range []string{ScenarioBoltStreaming, ScenarioBoltStreamingStall} {
		sc, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("scenario %q is not registered", name)
		}
		ctx, cancel := context.WithTimeout(context.Background(), boltStreamArmTimeout)
		report, err := sc.Run(ctx, sc.DefaultSeed)
		cancel()
		if err != nil {
			t.Fatalf("scenario %q: %v", name, err)
		}
		if report != nil {
			t.Fatalf("scenario %q reported violations at its default seed: %s", name, report)
		}
	}
}

// TestBoltStreamSemantics_SoakSweep drives the deterministic battery across many
// seeds. It is the long arm: the short layer runs a handful of seeds, and the sweep
// that would catch a draw-dependent gap runs behind the soak gate.
func TestBoltStreamSemantics_SoakSweep(t *testing.T) {
	testlayers.RequireSoak(t)
	defer goleak.VerifyNone(t)
	// 400 seeds: the sweep size rmp #2554 used to classify a draw-dependent gap,
	// and cheap enough here that the whole sweep costs about a second.
	const seeds = 400
	for i := uint64(0); i < seeds; i++ {
		seed := 0x2484_0000 + i
		ctx, cancel := context.WithTimeout(context.Background(), boltStreamArmTimeout)
		ev, err := RunBoltStreamSemantics(ctx, seed)
		cancel()
		if err != nil {
			t.Fatalf("seed %#x: %v", seed, err)
		}
		if v := checkBoltStreamSemantics(ev); len(v) > 0 {
			t.Fatalf("seed %#x contract violations %v\n%s", seed, v, ev)
		}
		if v := checkBoltStreamSemanticsNonVacuity(ev); len(v) > 0 {
			t.Fatalf("seed %#x coverage shortfall %v\n%s", seed, v, ev)
		}
	}
}
