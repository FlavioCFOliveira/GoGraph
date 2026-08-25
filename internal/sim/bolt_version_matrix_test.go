package sim

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// TestBoltVersionMatrix_Clean drives the whole matrix against a real credentialed
// server and asserts both the contract and the non-vacuity gate are satisfied.
func TestBoltVersionMatrix_Clean(t *testing.T) {
	defer goleak.VerifyNone(t)

	ev, err := RunBoltVersionMatrix(context.Background(), boltVersionMatrixDefaultSeed)
	if err != nil {
		t.Fatalf("RunBoltVersionMatrix: %v", err)
	}
	for _, v := range checkBoltVersionMatrix(ev) {
		t.Errorf("contract violation: %s %s: %s", v.Kind, v.Op, v.Message)
	}
	for _, v := range checkBoltVersionMatrixNonVacuity(ev) {
		t.Errorf("non-vacuity violation: %s %s: %s", v.Kind, v.Op, v.Message)
	}
	if t.Failed() {
		t.Log(ev.String())
	}
}

// TestBoltVersionMatrix_Deterministic asserts the same seed produces
// byte-identical evidence, which is what makes a failure replayable.
//
// It compares the RENDERING rather than the struct, which is sound only because
// the rendering was swept for everything not reachable from the seed — see
// [BoltVersionMatrixEvidence.String]. The one excluded class is spot-checked
// afterwards, so the sweep is asserted rather than trusted.
func TestBoltVersionMatrix_Deterministic(t *testing.T) {
	defer goleak.VerifyNone(t)

	const seed = 0x2486_D0D0
	first, err := RunBoltVersionMatrix(context.Background(), seed)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := RunBoltVersionMatrix(context.Background(), seed)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if first.String() != second.String() {
		t.Errorf("two runs of seed %#x rendered differently:\n--- first ---\n%s\n--- second ---\n%s",
			seed, first.String(), second.String())
	}
	// The excluded class must be excluded for the RIGHT reason: the bookmark text
	// really does differ between two runs in the same process, which is the
	// evidence that rendering it would have made this test flaky rather than
	// merely redundant.
	if len(first.Arms) > 0 && len(second.Arms) > 0 {
		a, b := first.Arms[0].TxBookmark, second.Arms[0].TxBookmark
		if a != "" && a == b {
			t.Errorf("two runs issued the SAME first bookmark %q; bookmarkCounter is process-global "+
				"(bolt/server/bookmark.go:13), so if it is now per-server the rendering could include the "+
				"literal text and this exclusion should be revisited", a)
		}
	}
}

// TestBoltVersionMatrix_ScenarioPasses drives the catalogue scenario end to end,
// so the registry wiring — not just the collector — is covered.
func TestBoltVersionMatrix_ScenarioPasses(t *testing.T) {
	defer goleak.VerifyNone(t)

	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioBoltVersionMatrix)
	if !ok {
		t.Fatalf("scenario %q not in catalogue", ScenarioBoltVersionMatrix)
	}
	rep, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("scenario run: %v", err)
	}
	if rep != nil {
		t.Errorf("scenario failed:\n%s", rep.String())
	}
}

// TestBoltVersionMatrix_TableIsCrossed pins the property that makes the matrix a
// crossed design rather than a list: the authentication column and the layout
// column must not be the same column.
//
// If every version that defers authentication were also the only one on the Bolt
// 5 layout, no observed difference could be attributed to ONE axis — 5.0 is the
// row that separates them, and dropping it would silently turn this scenario back
// into a 4.4-versus-5.6 comparison while every clause still passed.
func TestBoltVersionMatrix_TableIsCrossed(t *testing.T) {
	t.Parallel()

	var crossed int
	for _, tgt := range boltVersionTargets {
		if tgt.Major5Layout && !tgt.DefersAuth {
			crossed++
		}
	}
	if crossed == 0 {
		t.Fatalf("no target uses the Bolt 5 entity layout with INLINE authentication. Without such a row "+
			"(5.0) the auth axis and the encoding axis move together and no difference this scenario "+
			"measures is attributable to either. Targets: %+v", boltVersionTargets)
	}
	// And both sides of each axis must be present at all.
	var major4, deferred int
	for _, tgt := range boltVersionTargets {
		if !tgt.Major5Layout {
			major4++
		}
		if tgt.DefersAuth {
			deferred++
		}
	}
	if major4 == 0 || deferred == 0 {
		t.Fatalf("the matrix must hold at least one pre-5 layout target (got %d) and one deferred-auth "+
			"target (got %d)", major4, deferred)
	}
}

// TestBoltVersionMatrix_SeedMixDoesNotCancelTheDefaultSeed guards the one value
// [boltVersionSeedMix] may not take.
//
// XOR is self-annihilating, so a mix equal to a seed makes the mix a NO-OP at
// exactly that seed. When the seed in question is the catalogue default, the
// decorrelation the constant exists to provide is absent from the one run every
// report, sweep and reproduction starts from — and absent silently, because
// nothing else observes it. rmp #2485 shipped exactly that mistake
// (0x2485_B0_0C against a default of 0x2485_B00C, which Go reads as the same
// number), which is why the cheapest possible guard is worth having.
func TestBoltVersionMatrix_SeedMixDoesNotCancelTheDefaultSeed(t *testing.T) {
	t.Parallel()

	if effective := uint64(boltVersionMatrixDefaultSeed) ^ uint64(boltVersionSeedMix); effective == 0 {
		t.Fatalf("boltVersionSeedMix (%#x) equals boltVersionMatrixDefaultSeed (%#x), so the catalogue "+
			"default draws from NewSeed(0) and the mix decorrelates nothing on the one run every report "+
			"starts from; pick a mix that differs from the default seed",
			uint64(boltVersionSeedMix), uint64(boltVersionMatrixDefaultSeed))
	}
}

// TestWireClientHandshake_PreambleBytesAreUnchanged pins the exact 20 bytes each
// of the two pre-existing handshake spellings puts on the wire.
//
// rmp #2486 refactored both onto [WireClient.HandshakeOfferingSlots] so the
// preamble is built in ONE place. That refactor is only safe if it is
// byte-identical: [WireClient.Handshake] is what every other scenario in this
// package negotiates with, so a changed preamble would silently move every one of
// them onto a different protocol version — and it could do so without failing
// anything, because a range offer and an exact offer of the same top version
// negotiate the SAME result. Comparing negotiated versions could not see that;
// comparing bytes can.
//
// The expected bytes are written out literally, transcribed from the slot layout
// in bolt/proto/handshake.go:38-44, rather than recomputed from the primitive
// under test.
//
// It reads the bytes off a bare [SimListener] with no server behind it, so the
// preamble is captured exactly as written instead of being consumed by a
// negotiating peer.
func TestWireClientHandshake_PreambleBytesAreUnchanged(t *testing.T) {
	t.Parallel()

	magic := make([]byte, 4)
	binary.BigEndian.PutUint32(magic, proto.Magic)

	cases := []struct {
		send func(*WireClient)
		name string
		want []byte
	}{
		{
			name: "Handshake",
			send: func(c *WireClient) { _, _ = c.Handshake(context.Background()) },
			want: append(slices.Clone(magic),
				0, 6, 6, 5, // slot 0: major 5, minor 6, minor_range 6 (i.e. 5.0..5.6)
				0, 0, 4, 4, // slot 1: major 4, minor 4, no range
				0, 0, 0, 0,
				0, 0, 0, 0),
		},
		{
			name: "HandshakeOffering",
			send: func(c *WireClient) {
				_, _ = c.HandshakeOffering(context.Background(),
					proto.Version{Major: 5, Minor: 0}, proto.Version{Major: 4, Minor: 4})
			},
			want: append(slices.Clone(magic),
				0, 0, 0, 5, // slot 0: major 5, minor 0, no range
				0, 0, 4, 4, // slot 1: major 4, minor 4, no range
				0, 0, 0, 0,
				0, 0, 0, 0),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ln := NewSimListener(clock.Real())
			defer func() { _ = ln.Close() }()
			client, err := ln.Dial()
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			peer, err := ln.Accept()
			if err != nil {
				t.Fatalf("Accept: %v", err)
			}
			// The handshake blocks reading the 4-byte reply this bare listener will
			// never send, so it runs on its own goroutine and is abandoned when the
			// connection closes. Only the bytes it WROTE are under test.
			done := make(chan struct{})
			go func() {
				defer close(done)
				tc.send(NewWireClient(client, clock.Real()))
			}()
			got := make([]byte, 20)
			if _, err := io.ReadFull(peer, got); err != nil {
				t.Fatalf("read preamble: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Errorf("%s wrote preamble % x, want % x; the refactor onto HandshakeOfferingSlots changed "+
					"the bytes every other scenario in this package negotiates with", tc.name, got, tc.want)
			}
			_ = client.Close()
			_ = peer.Close()
			<-done
		})
	}
}

// TestDecodeBoltWire_ReadsHandBuiltBytes pins the INDEPENDENT PackStream reader
// against bytes written out by hand.
//
// The reader is the oracle for every encoding clause, so it must itself be
// falsifiable against something other than the encoder it adjudicates. Every
// byte string below is assembled here from the marker table, and every expected
// value is stated directly — nothing in this test calls the module's encoder or
// decoder, so a reader that agreed with a broken encoder would still fail here.
func TestDecodeBoltWire_ReadsHandBuiltBytes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		want  any
		name  string
		bytes []byte
	}{
		{name: "null", bytes: []byte{0xC0}, want: nil},
		{name: "false", bytes: []byte{0xC2}, want: false},
		{name: "true", bytes: []byte{0xC3}, want: true},
		{name: "tiny-int-0", bytes: []byte{0x00}, want: int64(0)},
		{name: "tiny-int-127", bytes: []byte{0x7F}, want: int64(127)},
		{name: "tiny-int-minus-1", bytes: []byte{0xFF}, want: int64(-1)},
		{name: "tiny-int-minus-16", bytes: []byte{0xF0}, want: int64(-16)},
		{name: "int8", bytes: []byte{0xC8, 0x80}, want: int64(-128)},
		{name: "int16", bytes: []byte{0xC9, 0x00, 0x8C}, want: int64(140)},
		{name: "int16-negative", bytes: []byte{0xC9, 0xFF, 0x74}, want: int64(-140)},
		{name: "int32", bytes: []byte{0xCA, 0x5E, 0x0D, 0x41, 0x85}, want: int64(1577927045)},
		{
			name:  "int64",
			bytes: []byte{0xCB, 0x00, 0x00, 0x0A, 0x0B, 0x9D, 0x4D, 0x32, 0x06},
			want:  int64(11045000000006),
		},
		{
			// 2.5 is 0x4004000000000000 in IEEE-754 binary64.
			name:  "float",
			bytes: []byte{0xC1, 0x40, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
			want:  2.5,
		},
		{name: "tiny-string", bytes: []byte{0x83, 'a', 'b', 'c'}, want: "abc"},
		{name: "empty-string", bytes: []byte{0x80}, want: ""},
		{name: "string8", bytes: append([]byte{0xD0, 0x03}, 'x', 'y', 'z'), want: "xyz"},
		{name: "bytes8", bytes: []byte{0xCC, 0x02, 0xDE, 0xAD}, want: []byte{0xDE, 0xAD}},
		{name: "tiny-list", bytes: []byte{0x92, 0x01, 0x02}, want: []any{int64(1), int64(2)}},
		{name: "empty-list", bytes: []byte{0x90}, want: []any{}},
		{
			name:  "tiny-map",
			bytes: []byte{0xA1, 0x81, 'k', 0x07},
			want:  map[string]any{"k": int64(7)},
		},
		{
			// A Bolt 4 Node: 'N' with three fields — id 140, labels ["P"], props {}.
			name:  "node-bolt-4",
			bytes: []byte{0xB3, 0x4E, 0xC9, 0x00, 0x8C, 0x91, 0x81, 'P', 0xA0},
			want: boltWireStruct{Tag: 0x4E, Fields: []any{
				int64(140), []any{"P"}, map[string]any{},
			}},
		},
		{
			// The same Node at the Bolt 5 layout: a fourth field, the element_id.
			name: "node-bolt-5",
			bytes: []byte{
				0xB4, 0x4E, 0xC9, 0x00, 0x8C, 0x91, 0x81, 'P', 0xA0, 0x83, '1', '4', '0',
			},
			want: boltWireStruct{Tag: 0x4E, Fields: []any{
				int64(140), []any{"P"}, map[string]any{}, "140",
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := decodeBoltWire(tc.bytes)
			if err != nil {
				t.Fatalf("decodeBoltWire(% x): %v", tc.bytes, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("decodeBoltWire(% x) = %T %#v, want %T %#v", tc.bytes, got, got, tc.want, tc.want)
			}
		})
	}
}

// TestDecodeBoltWire_RejectsMalformed pins the reader's refusals. A reader that
// silently tolerated a truncation or an unknown marker could report a short
// struct census as a correct one, which is precisely the verdict the layout
// clauses depend on.
func TestDecodeBoltWire_RejectsMalformed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		bytes []byte
	}{
		{name: "empty", bytes: nil},
		{name: "truncated-int16", bytes: []byte{0xC9, 0x00}},
		{name: "truncated-string", bytes: []byte{0x83, 'a'}},
		{name: "truncated-list", bytes: []byte{0x92, 0x01}},
		{name: "struct-without-tag", bytes: []byte{0xB1}},
		{name: "struct-missing-field", bytes: []byte{0xB2, 0x4E, 0x01}},
		{name: "unassigned-marker-0xC4", bytes: []byte{0xC4}},
		{name: "unassigned-marker-0xD3", bytes: []byte{0xD3}},
		{name: "big-struct-marker-0xDC", bytes: []byte{0xDC, 0x10, 0x4E}},
		{name: "non-string-map-key", bytes: []byte{0xA1, 0x01, 0x02}},
		{name: "trailing-bytes", bytes: []byte{0x01, 0x02}},
		{name: "size-beyond-message", bytes: []byte{0xD0, 0xFF, 'a'}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := decodeBoltWire(tc.bytes)
			if err == nil {
				t.Errorf("decodeBoltWire(% x) accepted malformed input as %#v; a reader that tolerates this "+
					"could report a truncated struct census as a correct one", tc.bytes, got)
			}
		})
	}
}

// TestDecodeBoltWire_RefusesOverDeepNesting pins the recursion bound. The reader
// is fed raw bytes off a socket, so an unbounded recursive decoder is a
// stack-overflow — a fatal, unrecoverable crash — waiting for a hostile peer.
func TestDecodeBoltWire_RefusesOverDeepNesting(t *testing.T) {
	t.Parallel()

	// A chain of single-element lists, one deeper than the bound allows.
	deep := make([]byte, 0, boltWireMaxDepth+2)
	for i := 0; i <= boltWireMaxDepth+1; i++ {
		deep = append(deep, 0x91)
	}
	deep = append(deep, 0x00)
	if _, err := decodeBoltWire(deep); err == nil {
		t.Errorf("decodeBoltWire accepted %d levels of nesting; the bound is %d", len(deep)-1, boltWireMaxDepth)
	}
	// And the bound is not so tight that a real Path is refused: the deepest value
	// this server emits is struct -> list -> struct -> map -> scalar.
	shallow := []byte{0x91, 0x91, 0x91, 0x01}
	if _, err := decodeBoltWire(shallow); err != nil {
		t.Errorf("decodeBoltWire refused four shallow levels: %v", err)
	}
}

// TestBoltWireCensus_IsIndependentOfMapOrder pins that the struct census does not
// depend on Go's randomised map iteration order.
//
// The census is compared byte for byte across arms and across runs. A walk that
// visited a property map in iteration order would make the census — and therefore
// the determinism clause and the layout clause — a measurement of the Go runtime
// rather than of the server, and it would do so intermittently.
func TestBoltWireCensus_IsIndependentOfMapOrder(t *testing.T) {
	t.Parallel()

	value := boltWireStruct{Tag: 0x4E, Fields: []any{
		int64(1),
		[]any{"L"},
		map[string]any{
			"a": boltWireStruct{Tag: 0x41, Fields: []any{int64(1)}},
			"b": boltWireStruct{Tag: 0x42, Fields: []any{int64(1), int64(2)}},
			"c": boltWireStruct{Tag: 0x43, Fields: []any{int64(1), int64(2), int64(3)}},
		},
	}}
	first := boltWireRenderCensus(boltWireCensus(value))
	for i := 0; i < 64; i++ {
		if got := boltWireRenderCensus(boltWireCensus(value)); got != first {
			t.Fatalf("census varied across walks of the same value: %q then %q", first, got)
		}
	}
	if !strings.Contains(first, "A/1 B/2 C/3") {
		t.Errorf("census %q does not visit map values in sorted key order", first)
	}
}

// cleanBoltVersionEvidence returns a hand-built evidence value that BOTH checkers
// pass, so a perturbation test can attribute any violation to the field it
// changed.
//
// It is built from the SAME rosters and expectation helpers the collector and the
// checkers use, never from literals copied out of a passing run. A fixture built
// from literals drifts: a new negotiation case or a new temporal kind would leave
// it healthy while the real run diverged from it, and every perturbation below
// would then be probing a shape the server no longer produces.
func cleanBoltVersionEvidence() BoltVersionMatrixEvidence {
	refs, zoneOK := boltVersionComputeTemporalRefs()
	ev := BoltVersionMatrixEvidence{
		Seed:              1,
		Supported:         slices.Clone(boltVersionSupportedTripwire),
		TemporalRef:       refs,
		NamedZoneResolved: zoneOK,
		LiveMarkers:       map[string]int{},
		RecoveredMarkers:  map[string]int{},
	}
	for i := range boltVersionNegotiationCases {
		tc := &boltVersionNegotiationCases[i]
		ev.Negotiations = append(ev.Negotiations, BoltVersionNegotiation{
			Name:     tc.Name,
			Slots:    boltVersionRenderSlots(tc.Slots),
			Accepted: tc.Accept,
			Got:      tc.Want,
		})
	}
	for i := range boltVersionTargets {
		ev.Arms = append(ev.Arms, cleanBoltVersionArm(&boltVersionTargets[i], i, &refs))
		ev.LiveMarkers[boltVersionTargets[i].Name] = 1
		ev.RecoveredMarkers[boltVersionTargets[i].Name] = 1
	}
	return ev
}

// The fixture's entity ids. They are arbitrary but must be DISTINCT, because the
// element_id clause derives its expectation from them and equal ids would make a
// wrong mapping indistinguishable from a right one.
const (
	cleanVersionNodeID  = int64(140)
	cleanVersionDstID   = int64(165)
	cleanVersionRelID   = int64(1)
	cleanVersionBytes4  = 144
	cleanVersionBytes5  = 168
	cleanVersionBookmk  = bookmarkPrefix + "00000001"
	cleanVersionRelType = boltVersionRelType
)

// cleanBoltVersionArm builds one passing arm for the target.
func cleanBoltVersionArm(t *boltVersionTarget, ordinal int, refs *BoltVersionTemporalRefs) BoltVersionArm {
	a := BoltVersionArm{
		Name:      t.Name,
		Asked:     t.Version,
		Canonical: t.Version,
		Spelled:   t.Version,
		// A spelling that DIFFERS from the canonical one, because
		// `nv-seeded-spelling-differed` requires at least one arm to have drawn a
		// non-canonical preamble.
		Spelling:        boltVersionRenderSlots([4]BoltOffer{{}, {Version: t.Version}}),
		SpellingDiffers: true,
		TxCommitted:     true,
		TxBookmark:      cleanVersionBookmk,
		TxCountAfter:    ordinal + 1,
	}
	if t.DefersAuth {
		a.Auth = BoltVersionAuthProbes{
			HelloGoodKind:        boltVersionKindSuccess,
			RunAfterHelloKind:    boltVersionKindFailure,
			RunAfterHelloCode:    boltVersionStateGateCode,
			RunAfterHelloMessage: "illegal message *proto.Run " + boltVersionInStateAuthentication,
			HelloWrongKind:       boltVersionKindSuccess,
			WrongHelloConnAlive:  true,
			HelloBareKind:        boltVersionKindSuccess,
			LogonKind:            boltVersionKindSuccess,
			RunAfterLogonKind:    boltVersionKindSuccess,
			ResetAfterHelloKind:  boltVersionKindSuccess,
			RunAfterResetKind:    boltVersionKindFailure,
			RunAfterResetCode:    boltVersionStateGateCode,
			RunAfterResetMessage: "illegal message *proto.Run " + boltVersionInStateNegotiation,
		}
	} else {
		a.Auth = BoltVersionAuthProbes{
			HelloGoodKind:       boltVersionKindSuccess,
			RunAfterHelloKind:   boltVersionKindSuccess,
			HelloWrongKind:      boltVersionKindFailure,
			HelloWrongCode:      boltVersionUnauthorizedCode,
			WrongHelloConnAlive: false,
			HelloBareKind:       boltVersionKindFailure,
			HelloBareCode:       boltVersionUnauthorizedCode,
			LogonKind:           boltVersionKindClosed,
			RunAfterLogonKind:   boltVersionKindClosed,
			ResetAfterHelloKind: boltVersionKindSuccess,
			RunAfterResetKind:   boltVersionKindSuccess,
		}
	}
	a.Entity = cleanBoltVersionEntity(t.Major5Layout)
	for _, kind := range boltVersionTemporalKinds {
		tag, ints, zone := boltVersionExpectTemporal(kind, t.Major5Layout, refs)
		a.Temporals = append(a.Temporals, BoltVersionTemporal{Kind: kind, Tag: tag, Ints: ints, Zone: zone})
	}
	a.Params = make([]any, len(boltVersionParamCases))
	for i, pc := range boltVersionParamCases {
		a.Params[i] = pc.Value
	}
	for _, ab := range boltVersionAbuseCases {
		a.Abuse = append(a.Abuse, BoltVersionAbuse{
			Name: ab.Name, Kind: boltVersionKindFailure, Code: ab.Code, Message: ab.Message,
		})
	}
	return a
}

// cleanBoltVersionEntity builds a passing entity capture for one layout.
func cleanBoltVersionEntity(major5 bool) BoltVersionEntity {
	census := boltVersionExpectedWidths(major5)
	ent := BoltVersionEntity{
		Census:      census,
		CodecCensus: slices.Clone(census),
		Bytes:       cleanVersionBytes4,
		NodeID:      cleanVersionNodeID,
		NodeLabels:  []string{boltVersionFixtureLabel},
		NodeProps:   map[string]any{"name": boltVersionSrcName, "n": boltVersionSrcN},
		RelID:       cleanVersionRelID,
		RelStart:    cleanVersionNodeID,
		RelEnd:      cleanVersionDstID,
		RelType:     cleanVersionRelType,
		RelProps:    map[string]any{"w": boltVersionRelW},
		PathNodeIDs: []int64{cleanVersionNodeID, cleanVersionDstID},
		PathRelIDs:  []int64{cleanVersionRelID},
		PathIndices: []int64{1, 1},
	}
	if !major5 {
		return ent
	}
	ent.Bytes = cleanVersionBytes5
	for _, id := range []int64{
		cleanVersionNodeID, cleanVersionRelID, cleanVersionNodeID, cleanVersionDstID,
		cleanVersionNodeID, cleanVersionDstID, cleanVersionRelID,
	} {
		ent.ElementIDs = append(ent.ElementIDs, strconv.FormatInt(id, 10))
	}
	return ent
}

// versionArmIndex returns the index of the named arm, or -1.
func versionArmIndex(e *BoltVersionMatrixEvidence, name string) int {
	for i := range e.Arms {
		if e.Arms[i].Name == name {
			return i
		}
	}
	return -1
}

// TestBoltVersionMatrix_OracleCanFail proves every contract clause CAN fire.
//
// A clause that cannot fail proves nothing, and a scenario whose green run is
// green because nothing is asserted is worse than no scenario at all: it reports
// coverage it does not have. Each case below perturbs exactly one field of an
// otherwise-clean evidence and names the clause that must catch it.
func TestBoltVersionMatrix_OracleCanFail(t *testing.T) {
	t.Parallel()

	base := cleanBoltVersionEvidence()
	if v := checkBoltVersionMatrix(&base); len(v) != 0 {
		t.Fatalf("clean evidence must pass the contract, got %d violation(s): %v", len(v), v)
	}
	if v := checkBoltVersionMatrixNonVacuity(&base); len(v) != 0 {
		t.Fatalf("clean evidence must pass non-vacuity, got %d violation(s): %v", len(v), v)
	}

	cases := []struct {
		mutate func(*BoltVersionMatrixEvidence)
		name   string
		wantOp string
	}{
		// --- negotiation ---
		{
			name: "the server's advertised version list changed under the expectation table",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Supported = append(e.Supported, proto.Version{Major: 5, Minor: 7})
			},
			wantOp: "negotiate-supported-list",
		},
		{
			name: "a preamble that must be refused was ACCEPTED",
			mutate: func(e *BoltVersionMatrixEvidence) {
				i := negotiationIndex(e, "unsupported-minor-4.3")
				e.Negotiations[i].Accepted, e.Negotiations[i].Got = true, proto.Version{Major: 4, Minor: 4}
			},
			wantOp: "negotiate-outcome",
		},
		{
			name: "a range offer resolved to the wrong version",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Negotiations[negotiationIndex(e, "range-5.2-down-to-5.0")].Got = proto.Version{Major: 5, Minor: 6}
			},
			wantOp: "negotiate-outcome",
		},
		{
			name: "the client's slot order decided the version instead of the server's preference",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Negotiations[negotiationIndex(e, "legacy-offered-first-loses")].Got = proto.Version{Major: 4, Minor: 4}
			},
			wantOp: "negotiate-outcome",
		},
		{
			name:   "the 4-byte reply never arrived",
			mutate: func(e *BoltVersionMatrixEvidence) { e.Negotiations[0].ReadErr = "reply-unreadable" },
			wantOp: "negotiate-reply-arrived",
		},
		// --- the anti-trap clause ---
		{
			name: "an arm believed it negotiated 4.4 and actually got 5.6",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Arms[versionArmIndex(e, "4.4")].Canonical = proto.Version{Major: 5, Minor: 6}
			},
			wantOp: "arm-negotiated-version",
		},
		{
			name: "the seeded spelling reached a different version from the canonical one",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Arms[versionArmIndex(e, "5.0")].Spelled = proto.Version{Major: 5, Minor: 6}
			},
			wantOp: "arm-spelling-invariant",
		},
		// --- the authentication split ---
		{
			name: "a RUN was served before LOGON on a deferred-auth version",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Arms[versionArmIndex(e, "5.6")].Auth.RunAfterHelloKind = boltVersionKindSuccess
			},
			wantOp: "logon-run-before-logon-refused",
		},
		{
			name: "the pre-LOGON refusal named the wrong state",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Arms[versionArmIndex(e, "5.1")].Auth.RunAfterHelloMessage = "illegal message *proto.Run in state READY"
			},
			wantOp: "logon-run-before-logon-refused",
		},
		{
			name: "an inline-auth version stopped serving a RUN straight after HELLO",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Arms[versionArmIndex(e, "4.4")].Auth.RunAfterHelloKind = boltVersionKindFailure
			},
			wantOp: "logon-run-after-inline-hello-served",
		},
		{
			name: "a WRONG password on HELLO was ACCEPTED at an inline-auth version",
			mutate: func(e *BoltVersionMatrixEvidence) {
				a := &e.Arms[versionArmIndex(e, "5.0")].Auth
				a.HelloWrongKind, a.HelloWrongCode = boltVersionKindSuccess, ""
			},
			wantOp: "logon-wrong-password-on-hello-refused",
		},
		{
			name: "a refused HELLO left the connection reusable for a second attempt",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Arms[versionArmIndex(e, "4.4")].Auth.WrongHelloConnAlive = true
			},
			wantOp: "logon-failed-hello-is-fatal",
		},
		{
			name: "a deferred-auth version started REFUSING a wrong password on HELLO",
			mutate: func(e *BoltVersionMatrixEvidence) {
				a := &e.Arms[versionArmIndex(e, "5.6")].Auth
				a.HelloWrongKind, a.HelloWrongCode = boltVersionKindFailure, boltVersionUnauthorizedCode
			},
			wantOp: "logon-wrong-password-on-hello-ignored",
		},
		{
			name: "a credential-less HELLO was admitted at an inline-auth version",
			mutate: func(e *BoltVersionMatrixEvidence) {
				a := &e.Arms[versionArmIndex(e, "4.4")].Auth
				a.HelloBareKind, a.HelloBareCode = boltVersionKindSuccess, ""
			},
			wantOp: "logon-bare-hello-refused",
		},
		{
			name: "a credential-less HELLO was refused at a deferred-auth version",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Arms[versionArmIndex(e, "5.1")].Auth.HelloBareKind = boltVersionKindFailure
			},
			wantOp: "logon-bare-hello-accepted",
		},
		{
			name: "a correct LOGON did not reach a usable session",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Arms[versionArmIndex(e, "5.6")].Auth.RunAfterLogonKind = boltVersionKindFailure
			},
			wantOp: "logon-reaches-ready",
		},
		{
			name: "RESET on a pre-LOGON session minted a READY it had not earned",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Arms[versionArmIndex(e, "5.6")].Auth.RunAfterResetKind = boltVersionKindSuccess
			},
			wantOp: "logon-reset-before-logon-drops-to-negotiation",
		},
		{
			name: "RESET dropped an ALREADY authenticated session back to NEGOTIATION",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Arms[versionArmIndex(e, "5.0")].Auth.RunAfterResetKind = boltVersionKindFailure
			},
			wantOp: "logon-reset-keeps-an-authenticated-session",
		},
		{
			name:   "RESET itself was refused",
			mutate: func(e *BoltVersionMatrixEvidence) { e.Arms[0].Auth.ResetAfterHelloKind = boltVersionKindFailure },
			wantOp: "logon-reset-accepted",
		},
		// --- the encodings ---
		{
			name: "a Bolt 4 client was sent the Bolt 5 Node layout",
			mutate: func(e *BoltVersionMatrixEvidence) {
				ent := &e.Arms[versionArmIndex(e, "4.4")].Entity
				ent.Census = boltVersionExpectedWidths(true)
				ent.CodecCensus = slices.Clone(ent.Census)
			},
			wantOp: "encoding-struct-layout",
		},
		{
			name: "the Relationship lost one of its three element_id fields",
			mutate: func(e *BoltVersionMatrixEvidence) {
				ent := &e.Arms[versionArmIndex(e, "5.6")].Entity
				ent.Census[1].Arity = 7
				ent.CodecCensus[1].Arity = 7
			},
			wantOp: "encoding-struct-layout",
		},
		{
			name: "the independent reader and the module codec disagree about the same bytes",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Arms[versionArmIndex(e, "5.1")].Entity.CodecCensus[0].Arity = 3
			},
			wantOp: "encoding-walker-agrees-with-codec",
		},
		{
			name: "an element_id stopped being the decimal rendering of its own id",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Arms[versionArmIndex(e, "5.0")].Entity.ElementIDs[0] = "4:abcd:140"
			},
			wantOp: "encoding-element-id-is-the-decimal-id",
		},
		{
			name: "a Bolt 4 record carried an element_id",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Arms[versionArmIndex(e, "4.4")].Entity.ElementIDs = []string{"140"}
			},
			wantOp: "encoding-no-element-id-below-bolt-5",
		},
		{
			name: "the encoder ignored the negotiated version and emitted one layout to everyone",
			mutate: func(e *BoltVersionMatrixEvidence) {
				four := &e.Arms[versionArmIndex(e, "4.4")].Entity
				five := &e.Arms[versionArmIndex(e, "5.6")].Entity
				*four = cleanBoltVersionEntity(true)
				*five = cleanBoltVersionEntity(true)
			},
			wantOp: "encoding-differs-across-majors",
		},
		{
			name:   "the entity record could not be captured at all",
			mutate: func(e *BoltVersionMatrixEvidence) { e.Arms[0].Entity.Err = "RUN refused" },
			wantOp: "encoding-record-captured",
		},
		// --- version-invariant semantics ---
		{
			name: "two versions read a DIFFERENT graph from the same fixture",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Arms[versionArmIndex(e, "5.6")].Entity.RelEnd = 999
			},
			wantOp: "invariant-entity-core",
		},
		{
			name: "a path's traversal indices changed with the protocol version",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Arms[versionArmIndex(e, "5.1")].Entity.PathIndices = []int64{-1, 1}
			},
			wantOp: "invariant-entity-core",
		},
		{
			name: "an Integer parameter came back as the identically-rendered Float",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Arms[versionArmIndex(e, "4.4")].Params[paramIndex("int")] = float64(-1234567)
			},
			wantOp: "invariant-parameter-roundtrip",
		},
		{
			name: "a map parameter lost a key on one version",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Arms[versionArmIndex(e, "5.6")].Params[paramIndex("map")] = map[string]any{"k": int64(7)}
			},
			wantOp: "invariant-parameter-roundtrip",
		},
		{
			name:   "the explicit transaction did not commit",
			mutate: func(e *BoltVersionMatrixEvidence) { e.Arms[1].TxCommitted = false },
			wantOp: "invariant-explicit-transaction",
		},
		{
			name:   "COMMIT returned a malformed bookmark",
			mutate: func(e *BoltVersionMatrixEvidence) { e.Arms[2].TxBookmark = "bookmark-3" },
			wantOp: "invariant-commit-bookmark-shape",
		},
		{
			name:   "an earlier version's committed write went missing",
			mutate: func(e *BoltVersionMatrixEvidence) { e.Arms[3].TxCountAfter = 3 },
			wantOp: "invariant-marker-census-advances",
		},
		{
			name: "a malformed message drew a different refusal on one version",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Arms[versionArmIndex(e, "4.4")].Abuse[0].Message = "unrecognised message type"
			},
			wantOp: "invariant-abuse-refusal",
		},
		{
			name: "a wrong-state message was ACCEPTED",
			mutate: func(e *BoltVersionMatrixEvidence) {
				ab := &e.Arms[versionArmIndex(e, "5.0")].Abuse[1]
				ab.Kind, ab.Code, ab.Message = boltVersionKindSuccess, "", ""
			},
			wantOp: "invariant-abuse-refusal",
		},
		// --- temporal ---
		{
			name: "a 4.4 client was sent the Bolt 5 UTC datetime tag",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Arms[versionArmIndex(e, "4.4")].Temporals[temporalIndex("datetime-offset")].Tag = 0x49
			},
			wantOp: "temporal-zoned-datetime-convention",
		},
		{
			name: "a 4.4 client was sent the UTC epoch second under the legacy tag",
			mutate: func(e *BoltVersionMatrixEvidence) {
				tv := &e.Arms[versionArmIndex(e, "4.4")].Temporals[temporalIndex("datetime-offset")]
				tv.Ints = slices.Clone(tv.Ints)
				tv.Ints[0] -= boltVersionOffsetSeconds
			},
			wantOp: "temporal-zoned-datetime-convention",
		},
		{
			name: "a 5.x client was sent the legacy local seconds",
			mutate: func(e *BoltVersionMatrixEvidence) {
				tv := &e.Arms[versionArmIndex(e, "5.6")].Temporals[temporalIndex("datetime-named")]
				tv.Ints = slices.Clone(tv.Ints)
				tv.Ints[0] += boltVersionOffsetSeconds
			},
			wantOp: "temporal-zoned-datetime-convention",
		},
		{
			name: "the named-zone datetime lost its zone name",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Arms[versionArmIndex(e, "5.1")].Temporals[temporalIndex("datetime-named")].Zone = ""
			},
			wantOp: "temporal-zoned-datetime-convention",
		},
		{
			name: "a zone-LESS temporal started varying with the protocol version",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Arms[versionArmIndex(e, "4.4")].Temporals[temporalIndex("localdatetime")].Tag = 0x65
			},
			wantOp: "temporal-version-invariant",
		},
		{
			name: "the duration's fields were reordered",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Arms[0].Temporals[temporalIndex("duration")].Ints = []int64{2, 1, 3, 4}
			},
			wantOp: "temporal-version-invariant",
		},
		// --- durability ---
		{
			name:   "a version's committed marker is not in the live engine",
			mutate: func(e *BoltVersionMatrixEvidence) { e.LiveMarkers["5.0"] = 0 },
			wantOp: "durable-marker-live",
		},
		{
			name:   "a committed write did not survive real WAL recovery",
			mutate: func(e *BoltVersionMatrixEvidence) { e.RecoveredMarkers["4.4"] = 0 },
			wantOp: "durable-survives-recovery",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev := cleanBoltVersionEvidence()
			tc.mutate(&ev)
			assertVersionClauseFired(t, checkBoltVersionMatrix(&ev), tc.wantOp)
		})
	}
}

// negotiationIndex returns the index of the named negotiation case.
func negotiationIndex(e *BoltVersionMatrixEvidence, name string) int {
	for i := range e.Negotiations {
		if e.Negotiations[i].Name == name {
			return i
		}
	}
	return -1
}

// paramIndex returns the index of the named parameter case.
func paramIndex(name string) int {
	for i := range boltVersionParamCases {
		if boltVersionParamCases[i].Name == name {
			return i
		}
	}
	return -1
}

// temporalIndex returns the index of the named temporal kind.
func temporalIndex(name string) int { return slices.Index(boltVersionTemporalKinds, name) }

// assertVersionClauseFired requires the named clause — and no other — to have
// fired.
//
// It insists the EXPECTED clause is among the violations rather than merely that
// something failed: a perturbation caught by an unrelated clause would leave the
// intended one unproven while the test still passed.
func assertVersionClauseFired(t *testing.T, got []Violation, wantOp string) {
	t.Helper()
	want := boltVersionOp(wantOp)
	for _, v := range got {
		if v.Op == want {
			return
		}
	}
	if len(got) == 0 {
		t.Fatalf("no clause fired; %s was expected to catch this perturbation", want)
	}
	ops := make([]string, len(got))
	for i, v := range got {
		ops[i] = fmt.Sprintf("%s: %s", v.Op, v.Message)
	}
	t.Fatalf("expected %s to fire, but the violations were:\n  %s", want, strings.Join(ops, "\n  "))
}

// TestBoltVersionMatrix_NonVacuityCanFail proves every shortfall clause CAN fire.
//
// The non-vacuity family answers a different question from the contract — was the
// run in a POSITION to notice — so it needs its own falsifiability table. A
// shortfall clause that could not fire would let a run that reached only one
// protocol version report a clean version matrix.
func TestBoltVersionMatrix_NonVacuityCanFail(t *testing.T) {
	t.Parallel()

	cases := []struct {
		mutate func(*BoltVersionMatrixEvidence)
		name   string
		wantOp string
	}{
		{
			name: "an arm never confirmed the version it negotiated",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Arms[versionArmIndex(e, "4.4")].NegotiateErr = "canonical-rejected"
			},
			wantOp: "nv-every-target-negotiated",
		},
		{
			name: "the crossed design collapsed — no inline-auth arm on the Bolt 5 layout",
			mutate: func(e *BoltVersionMatrixEvidence) {
				e.Arms[versionArmIndex(e, "5.0")].Spelled = proto.Version{Major: 5, Minor: 6}
			},
			wantOp: "nv-crossed-design-constructed",
		},
		{
			name: "only one arm captured an entity record",
			mutate: func(e *BoltVersionMatrixEvidence) {
				for i := 1; i < len(e.Arms); i++ {
					e.Arms[i].Entity.Err = "capture: RUN refused"
				}
			},
			wantOp: "nv-entity-records-captured",
		},
		{
			name: "every arm saw the same entity arities, so the layout branch was never taken",
			mutate: func(e *BoltVersionMatrixEvidence) {
				for i := range e.Arms {
					e.Arms[i].Entity.Census = boltVersionExpectedWidths(true)
					e.Arms[i].Entity.CodecCensus = slices.Clone(e.Arms[i].Entity.Census)
				}
			},
			wantOp: "nv-layout-branch-observed",
		},
		{
			name: "only one zoned-datetime convention was ever observed",
			mutate: func(e *BoltVersionMatrixEvidence) {
				i := versionArmIndex(e, "4.4")
				e.Arms[i].Temporals[temporalIndex("datetime-offset")].Tag = 0x49
				e.Arms[i].Temporals[temporalIndex("datetime-named")].Tag = 0x69
			},
			wantOp: "nv-temporal-branch-observed",
		},
		{
			name:   "the zone database could not resolve the named zone",
			mutate: func(e *BoltVersionMatrixEvidence) { e.NamedZoneResolved = false },
			wantOp: "nv-named-zone-reference-available",
		},
		{
			name: "the negotiation table never saw a refusal",
			mutate: func(e *BoltVersionMatrixEvidence) {
				for i := range e.Negotiations {
					e.Negotiations[i].Accepted = true
				}
			},
			wantOp: "nv-negotiation-table-exercised",
		},
		{
			name: "the seed drew the canonical spelling for every arm",
			mutate: func(e *BoltVersionMatrixEvidence) {
				for i := range e.Arms {
					e.Arms[i].SpellingDiffers = false
				}
			},
			wantOp: "nv-seeded-spelling-differed",
		},
		{
			name:   "a version wrote no marker, so the durable census cannot attribute one to it",
			mutate: func(e *BoltVersionMatrixEvidence) { e.Arms[2].TxCommitted = false },
			wantOp: "nv-every-version-committed",
		},
		{
			name:   "the parameter matrix was only partly round-tripped",
			mutate: func(e *BoltVersionMatrixEvidence) { e.Arms[0].Params = e.Arms[0].Params[:2] },
			wantOp: "nv-parameter-matrix-round-tripped",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev := cleanBoltVersionEvidence()
			tc.mutate(&ev)
			assertVersionClauseFired(t, checkBoltVersionMatrixNonVacuity(&ev), tc.wantOp)
		})
	}
}

// TestBoltVersionMatrix_ExpectedWidthsDifferAcrossMajors pins the one property
// the whole encoding family rests on: the two layouts are not the same layout.
//
// If [boltVersionExpectedWidths] ever returned the same census for both majors —
// a plausible outcome of a careless edit — every layout clause would still pass,
// `encoding-differs-across-majors` would fire on the real run, and the reason
// would be invisible. Stating it here localises that edit to one line.
func TestBoltVersionMatrix_ExpectedWidthsDifferAcrossMajors(t *testing.T) {
	t.Parallel()

	four, five := boltVersionExpectedWidths(false), boltVersionExpectedWidths(true)
	if slices.Equal(four, five) {
		t.Fatalf("the pre-5 and 5+ expected layouts are identical: [%s]", boltWireRenderCensus(four))
	}
	// The path structure itself must be the CONTROL: three fields at both.
	for i, w := range four {
		if w.Tag != boltTagPath {
			continue
		}
		if five[i] != w {
			t.Errorf("the Path structure changed arity across majors (%s vs %s); the version reaches a path "+
				"only through the members it contains", w, five[i])
		}
	}
}

// TestBoltVersionMatrix_TemporalReferenceOffsetIsNonZero pins that the zoned
// instants the scenario asks for are at a NON-ZERO offset from UTC.
//
// The Bolt 4 legacy convention writes utcEpochSec + offsetSec where Bolt 5 writes
// utcEpochSec. At a zone whose offset is zero the two produce the IDENTICAL
// seconds field, and `temporal-zoned-datetime-convention` degenerates into a
// tag-only check while still reading green. Europe/Lisbon in January is exactly
// that trap — WET, offset 0 — and was the first zone tried.
func TestBoltVersionMatrix_TemporalReferenceOffsetIsNonZero(t *testing.T) {
	t.Parallel()

	refs, ok := boltVersionComputeTemporalRefs()
	if refs.OffsetSeconds == 0 {
		t.Errorf("the offset-zone datetime is at UTC+00:00, so the legacy and UTC conventions encode the " +
			"same seconds field and the clause cannot tell them apart")
	}
	if !ok {
		t.Skipf("no zone database for %q; the named-zone half of this check cannot be constructed "+
			"(the run itself reports this as nv-named-zone-reference-available)", boltVersionZoneName)
	}
	if refs.Named.OffsetSeconds == 0 {
		t.Errorf("%s resolves to UTC+00:00 at the instant the query asks for, so the named-zone clause "+
			"degenerates to a tag-only check; pick a zone and date with a non-zero offset",
			boltVersionZoneName)
	}
	// And the two conventions must therefore produce different seconds.
	_, legacy, _ := boltVersionExpectTemporal("datetime-offset", false, &refs)
	_, utc, _ := boltVersionExpectTemporal("datetime-offset", true, &refs)
	if legacy[0] == utc[0] {
		t.Errorf("the legacy and UTC conventions expect the same seconds field (%d)", legacy[0])
	}
}
