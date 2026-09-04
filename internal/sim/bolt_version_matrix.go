package sim

// bolt_version_matrix.go — the Bolt protocol version matrix under simulation (rmp #2486).
//
// # The gap this closes
//
// Every DST connection before this task negotiated 5.6. [WireClient.Handshake] offers
// 5.6 with a minor range down to 5.0 in slot 0 and 4.4 in slot 1, and the server picks
// the highest version it supports inside any offered range, so 5.6 is what every arm
// of every scenario got. rmp #2481 added [WireClient.HandshakeOffering] to reach a
// specific version, and used it — but only to reach 5.0, and only to check that a
// credential-bearing HELLO is accepted there. Nothing had ever driven 4.4 at all, and
// nothing had ever compared one version's behaviour against another's.
//
// That left two whole axes of the server undriven, and they are DIFFERENT axes:
//
//   - the ENTITY and TEMPORAL encodings branch on the MAJOR version. A Node is three
//     fields at Bolt 4 and four at Bolt 5 (bolt/server/entity_struct.go:96-98), a
//     Relationship five and eight (:112-118), an UnboundRelationship three and four
//     (:130-132), and a Path inherits all of them by recursion (:144-152). A zoned
//     DateTime switches BOTH its struct tag and the MEANING of its seconds field:
//     0x49/0x69 carrying a true UTC epoch second at major >= 5, 0x46/0x66 carrying the
//     wall-clock instant expressed as if UTC at 4.4 (dateTimeToPackstream,
//     bolt/server/session.go:2222-2243).
//   - AUTHENTICATION branches on the MINOR version, at a different place.
//     authDeferredToLogon compares against proto.Version{5, 1}
//     (bolt/server/state.go:294-305), so Bolt >= 5.1 sends a credential-less HELLO and
//     authenticates on a separate LOGON, while <= 5.0 carries the credentials on HELLO
//     itself and reaches READY directly.
//
// The task text calls the second axis "4.4 (no LOGON)". Reading the code says more
// than that: 5.0 is on the SAME side of the auth split as 4.4 and on the OTHER side of
// the encoding split. That is why 5.0 is called out separately as never entered, and
// it is what makes the matrix a CROSSED design rather than a list:
//
//	              inline auth (HELLO)   deferred auth (LOGON)
//	  Bolt 4 layout      4.4                    —
//	  Bolt 5 layout      5.0                  5.1, 5.6
//
// 4.4 against 5.0 moves the encoding axis with the auth axis held fixed; 5.0 against
// 5.1 moves the auth axis with the encoding axis held fixed. Either difference is
// therefore attributable to one axis, which a 4.4-versus-5.6 comparison alone could
// never be.
//
// # The load-bearing shape: semantics INVARIANT, encodings DIFFERENT
//
// The whole scenario rests on asserting BOTH halves, because either half alone is
// satisfiable by a broken server:
//
//   - A run that only required the DECODED values to agree across versions would pass
//     against a server that ignored the negotiated version entirely and emitted Bolt 5
//     structures to a 4.4 client — the values would agree perfectly, and a real 4.4
//     driver would fail to hydrate them (its hydrator asserts the field count).
//   - A run that only required the encodings to differ would pass against a server
//     that corrupted the data on one branch.
//
// So the checkers state both, and `encoding-differs-across-versions` is written
// deliberately as the guard on the first: the SAME query's RECORD, captured at 4.4 and
// at 5.6, must not produce the same struct census or the same byte length. If a future
// change made the encoder version-blind, every "decoded values are equal" clause would
// still pass and that one clause would fire.
//
// # The oracles are independent references, not the server's own codec
//
// Two independent references adjudicate the encodings, because the interesting claims
// are about BYTES:
//
//   - [decodeBoltWire] is a minimal PackStream reader written in this file from the
//     marker table, and used to obtain each RECORD's struct census — every (tag, arity)
//     pair in encounter order — from the raw chunked bytes. Its marker table was
//     derived from real captures of this server and then confirmed against the
//     published marker constants (bolt/packstream/encoder.go:24-59);
//     [TestDecodeBoltWire_ReadsHandBuiltBytes] pins it against hand-written bytes whose
//     content is known independently of any encoder, so the reference is itself
//     falsifiable.
//   - the value-level oracles are computed by the HARNESS: the entity ids and property
//     maps are what this scenario itself created, the element_id strings are
//     strconv.FormatInt of those ids, and every temporal field is computed with Go's
//     own time package from the literal the query asked for.
//
// `encoding-walker-agrees-with-codec` then runs the module's own decoder over the same
// bytes and requires the two censuses to match, so a bug in the independent reader
// surfaces as a disagreement rather than as a silent wrong verdict.
//
// # What the no-LOGON contract actually is, MEASURED against a credentialed server
//
// The server this scenario runs is [server.BasicAuthHandler] with one principal and
// one password, never [server.NoAuthHandler]: against a handler that admits everyone,
// "the credentials were accepted" is true for every version and proves nothing. With a
// handler that genuinely refuses, the SAME bytes produce OPPOSITE outcomes on the two
// sides of 5.1, and that is what makes the contract falsifiable:
//
//   - a WRONG password on HELLO draws Neo.ClientError.Security.Unauthorized and the
//     connection is TORN DOWN at 4.4 and 5.0, and draws SUCCESS with the connection
//     intact at 5.1 and 5.6 — because HELLO does not authenticate there at all;
//   - a credential-LESS HELLO is refused Unauthorized at 4.4 and 5.0, and succeeds at
//     5.1 and 5.6;
//   - a RUN sent immediately after a successful HELLO is SERVED at 4.4 and 5.0, and is
//     refused at 5.1 and 5.6 by the state gate, which names the state it refused from:
//     "illegal message *proto.Run in state AUTHENTICATION";
//   - a RESET sent on that pre-LOGON session returns it to NEGOTIATION rather than to
//     READY — the deliberate pre-authentication RESET gate of task #1345
//     (bolt/server/state.go:124-133 and Session.handleReset's !s.authenticated branch,
//     bolt/server/session.go:1038-1041) — so the following RUN is refused naming
//     NEGOTIATION, while the same RESET on an inline-auth session leaves it usable.
//
// The refusal clauses pin the whole `in state X` phrase and not the bare state name,
// following rmp #2484: a containment check on a state name alone accepts a refusal
// from a different state whose name contains it.
//
// # The offer SPELLING is seed-chosen, and its invariance is the claim
//
// Each arm negotiates its target version TWICE on two connections: once with the
// canonical spelling (the exact version in slot 0, minor range 0) and once with a
// spelling drawn from the seed — a different SLOT, a minor RANGE whose top is the
// target, and optionally a decoy offer for a version this server does not support.
// Both must land on the same version. The invariant is that the negotiated version is
// a function of what the client offered and not of how it spelled it, which is a real
// property of Negotiate's two nested loops (bolt/proto/handshake.go:109-132): it scans
// SupportedVersions highest-first in the OUTER loop, so the client's slot order cannot
// influence the choice.
//
// Nothing else here is seeded. The scenario is a fixed script otherwise, so the
// evidence rendering is a function of the seed alone — with the one exception every
// Bolt scenario in this package shares: a bookmark's literal text comes from a
// PROCESS-GLOBAL counter (bolt/server/bookmark.go:13), so it is rendered as a
// positional token and asserted only on its SHAPE and its strict advance, exactly as
// rmp #2485 does.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// -----------------------------------------------------------------------------
// The matrix
// -----------------------------------------------------------------------------

// boltVersionTarget is one row of the version matrix: a version to negotiate and
// the behaviour this scenario requires of it.
//
// DefersAuth and Major5Layout are LITERAL per row rather than computed from the
// version. Deriving them would restate the server's own two threshold rules
// (authDeferredToLogon and the `boltMajor >= 5` branches) and could not then
// disagree with them; written out, the table IS the oracle, and the fact that the
// two columns differ on exactly one row is what makes the design crossed rather
// than a list. [TestBoltVersionMatrix_TableIsCrossed] pins that property.
type boltVersionTarget struct {
	// Name is the arm's identifier in a violation message and in the rendering.
	Name string
	// Version is the version the arm negotiates.
	Version proto.Version
	// MaxMinorRange is the widest minor RANGE whose top is this version and which
	// still resolves to it. A range offer [minor-range, minor] cannot reach below
	// the major, and the server picks the highest supported version inside it, so
	// the top of the range decides the outcome and the width is free up to the
	// minor itself. It bounds the seeded spelling draw.
	MaxMinorRange uint8
	// DefersAuth is true when the version authenticates on LOGON rather than on
	// HELLO — Bolt >= 5.1.
	DefersAuth bool
	// Major5Layout is true when the version uses the Bolt 5 entity layout
	// (element_id fields) and the UTC zoned-datetime convention — major >= 5.
	Major5Layout bool
}

// boltVersionTargets is the matrix. 4.4 and 5.0 share the inline-auth column and
// differ in layout; 5.0 and 5.1 share the layout column and differ in auth; 5.6 is
// the version every other scenario in the package negotiates, carried here as the
// control that the matrix reproduces the status quo.
var boltVersionTargets = []boltVersionTarget{
	{Name: "4.4", Version: proto.Version{Major: 4, Minor: 4}, MaxMinorRange: 4, DefersAuth: false, Major5Layout: false},
	{Name: "5.0", Version: proto.Version{Major: 5, Minor: 0}, MaxMinorRange: 0, DefersAuth: false, Major5Layout: true},
	{Name: "5.1", Version: proto.Version{Major: 5, Minor: 1}, MaxMinorRange: 1, DefersAuth: true, Major5Layout: true},
	{Name: "5.6", Version: proto.Version{Major: 5, Minor: 6}, MaxMinorRange: 6, DefersAuth: true, Major5Layout: true},
}

// boltVersionSupportedTripwire is what this scenario believes the server
// advertises, as a literal.
//
// It is NOT a re-derivation of [proto.SupportedVersions] used to check the same
// list against itself: it is a TRIPWIRE on the expectation table below. Every
// entry of boltVersionNegotiationCases states an outcome that follows from this
// exact support list — "4.3 is refused", "a range topping out at 5.9 lands on
// 5.6" — so adding 5.7 or dropping 4.4 upstream would silently turn several of
// those literal expectations into statements about a server that no longer
// exists. Comparing the two lists makes that a loud failure at the one clause
// whose job is to notice.
var boltVersionSupportedTripwire = []proto.Version{
	{Major: 5, Minor: 6}, {Major: 5, Minor: 5}, {Major: 5, Minor: 4}, {Major: 5, Minor: 3},
	{Major: 5, Minor: 2}, {Major: 5, Minor: 1}, {Major: 5, Minor: 0},
	{Major: 4, Minor: 4},
}

// boltVersionCredentials is the single principal/password the scenario's server
// validates. A credentialed handler is the point: see the file comment.
const (
	boltVersionPrincipal = "matrix-driver"
	boltVersionPassword  = "corr3ct-h0rse-battery"
)

// boltVersionSeedMix decorrelates the SimDisk sub-seed from the scenario seed, so
// the disk's draw stream is independent of the offer-spelling draws. It must
// differ from [boltVersionMatrixDefaultSeed]: XOR is self-annihilating, so a mix
// equal to a seed is a no-op at exactly that seed, and at the catalogue default
// that is the one run every report starts from.
// [TestBoltVersionMatrix_SeedMixDoesNotCancelTheDefaultSeed] guards it.
const boltVersionSeedMix = 0x2486_5EED

// The labels and property keys the scenario writes. They are distinct from every
// other scenario's so a census cannot count another arm's node.
const (
	// boltVersionFixtureLabel marks the two nodes of the read fixture — the graph
	// every arm reads back to capture its entity encodings.
	boltVersionFixtureLabel = "VMFixture"
	// boltVersionRelType is the fixture relationship's type.
	boltVersionRelType = "VMLINK"
	// boltVersionMarkerLabel marks the node each arm commits in an explicit
	// transaction, so the durable census can attribute one node to each version.
	boltVersionMarkerLabel = "VMMarker"
	// boltVersionMarkerKey is the property carrying the arm's version name.
	boltVersionMarkerKey = "v"
)

// The fixture's two node names and the relationship weight. The scenario reads
// the ids off the wire rather than assuming them, so these are the only literals
// the entity oracle needs.
const (
	boltVersionSrcName = "src"
	boltVersionDstName = "dst"
	boltVersionRelW    = 2.5
	boltVersionSrcN    = int64(1)
)

// -----------------------------------------------------------------------------
// The independent PackStream reader
// -----------------------------------------------------------------------------

// boltWireStruct is a PackStream structure as the INDEPENDENT reader sees it: a
// signature byte and its fields, with no interpretation of what the signature
// means. It is deliberately a different type from [packstream.Struct] so a
// checker cannot accidentally compare the module's decoding of a record with
// itself.
type boltWireStruct struct {
	Fields []any
	Tag    byte
}

// boltWireWidth is the (tag, arity) pair the struct census records for one
// structure. Arity is the field count the MARKER declares, which is the number a
// driver's hydrator asserts, so it is the quantity the layout clauses are stated
// over.
type boltWireWidth struct {
	Tag   byte
	Arity int
}

// String renders a width as tag-and-arity, e.g. `N/4`, using the printable
// signature byte where there is one. It is used in violation messages and in the
// evidence rendering, so it must stay a pure function of the pair.
func (w boltWireWidth) String() string {
	if w.Tag >= 0x20 && w.Tag < 0x7F {
		return fmt.Sprintf("%c/%d", rune(w.Tag), w.Arity)
	}
	return fmt.Sprintf("0x%02X/%d", w.Tag, w.Arity)
}

// The Bolt structure signatures for graph entities, transcribed INDEPENDENTLY
// from the protocol (they are the ASCII letters the format names them by).
// bolt/server has its own unexported copies; duplicating them here rather than
// exporting those keeps the oracle from importing the encoder's own idea of what
// it emits, and keeps this task from touching bolt/ at all.
const (
	boltTagNode                = 0x4E // 'N'
	boltTagRelationship        = 0x52 // 'R'
	boltTagUnboundRelationship = 0x72 // 'r'
	boltTagPath                = 0x50 // 'P'
	boltTagRecord              = 0x71 // RECORD, the response message that carries them
)

// boltWireMaxDepth bounds recursion in the independent reader. The deepest value
// this server emits is a Path (struct → list → struct → map → scalar), so the
// bound is far above anything reachable; it exists because a reader that recurses
// on attacker-shaped input without a bound is a stack-overflow waiting to happen,
// and this one is fed raw bytes off a socket.
const boltWireMaxDepth = 32

// decodeBoltWire decodes one complete PackStream value from b, INDEPENDENTLY of
// [github.com/FlavioCFOliveira/GoGraph/bolt/packstream].
//
// It exists so a clause about the WIRE ENCODING is adjudicated by something other
// than the codec that produced it. The marker table below was derived from real
// hex captures of this server's records (a Node reading `b3 4e` at 4.4 and `b4 4e`
// at 5.0, an INT_16 reading `c9 00 8c`, a float reading `c1 40 04 ...`) and then
// confirmed against the published constants in bolt/packstream/encoder.go:24-59.
// It is pinned against hand-written bytes by
// [TestDecodeBoltWire_ReadsHandBuiltBytes].
//
// Decoded kinds map to Go as: null → nil, boolean → bool, every integer width →
// int64, float → float64, string → string, bytes → []byte, list → []any, map →
// map[string]any, structure → [boltWireStruct]. Trailing bytes after one complete
// value are an ERROR rather than ignored: a message that decodes and then has
// something left over is malformed, and silently accepting it would let a
// mis-decode read as a pass.
//
// A marker this reader does not know is an error, never a skip. PackStream v1
// leaves 0xC4–0xC7, 0xD3, 0xD7 and 0xDB–0xEF unassigned, and this server's encoder
// emits structures only in the tiny form 0xB0–0xBF (WriteStructHeader refuses a
// field count outside [0,15], bolt/packstream/encoder.go:334-340), so anything
// else arriving is a change worth failing on.
func decodeBoltWire(b []byte) (any, error) {
	r := &boltWireReader{b: b}
	v, err := r.value(0)
	if err != nil {
		return nil, err
	}
	if r.i != len(r.b) {
		return nil, fmt.Errorf("packstream: %d trailing byte(s) after a complete value", len(r.b)-r.i)
	}
	return v, nil
}

// boltWireReader is the cursor decodeBoltWire walks b with.
type boltWireReader struct {
	b []byte
	i int
}

// value decodes one value at the cursor.
//
// It carries one arm per PackStream marker family; splitting the table would hide
// the very thing the reader exists to make readable.
//
// the marker table is the point; see above.
func (r *boltWireReader) value(depth int) (any, error) {
	if depth > boltWireMaxDepth {
		return nil, fmt.Errorf("packstream: nesting deeper than %d at offset %d", boltWireMaxDepth, r.i)
	}
	m, err := r.u8()
	if err != nil {
		return nil, err
	}
	switch {
	case m <= 0x7F:
		return int64(m), nil // TINY_INT 0..127
	case m >= 0xF0:
		return int64(int8(m)), nil // TINY_INT -16..-1
	case m >= 0x80 && m <= 0x8F:
		return r.str(int(m & 0x0F))
	case m >= 0x90 && m <= 0x9F:
		return r.list(int(m&0x0F), depth)
	case m >= 0xA0 && m <= 0xAF:
		return r.dict(int(m&0x0F), depth)
	case m >= 0xB0 && m <= 0xBF:
		return r.structure(int(m&0x0F), depth)
	}
	switch m {
	case 0xC0:
		return nil, nil
	case 0xC1:
		bits, err := r.uN(8)
		if err != nil {
			return nil, err
		}
		return math.Float64frombits(bits), nil
	case 0xC2:
		return false, nil
	case 0xC3:
		return true, nil
	case 0xC8, 0xC9, 0xCA, 0xCB:
		return r.intN(1 << (m - 0xC8)) // 1, 2, 4, 8 bytes
	case 0xCC, 0xCD, 0xCE:
		n, err := r.sizeN(1 << (m - 0xCC))
		if err != nil {
			return nil, err
		}
		return r.bytes(n)
	case 0xD0, 0xD1, 0xD2:
		n, err := r.sizeN(1 << (m - 0xD0))
		if err != nil {
			return nil, err
		}
		return r.str(n)
	case 0xD4, 0xD5, 0xD6:
		n, err := r.sizeN(1 << (m - 0xD4))
		if err != nil {
			return nil, err
		}
		return r.list(n, depth)
	case 0xD8, 0xD9, 0xDA:
		n, err := r.sizeN(1 << (m - 0xD8))
		if err != nil {
			return nil, err
		}
		return r.dict(n, depth)
	}
	return nil, fmt.Errorf("packstream: unassigned marker 0x%02X at offset %d", m, r.i-1)
}

// u8 reads one byte.
func (r *boltWireReader) u8() (byte, error) {
	if r.i >= len(r.b) {
		return 0, fmt.Errorf("packstream: truncated at offset %d of %d", r.i, len(r.b))
	}
	v := r.b[r.i]
	r.i++
	return v, nil
}

// uN reads n big-endian bytes as an unsigned value. n is 1, 2, 4 or 8.
func (r *boltWireReader) uN(n int) (uint64, error) {
	if r.i+n > len(r.b) {
		return 0, fmt.Errorf("packstream: truncated %d-byte field at offset %d of %d", n, r.i, len(r.b))
	}
	var v uint64
	for _, c := range r.b[r.i : r.i+n] {
		v = v<<8 | uint64(c)
	}
	r.i += n
	return v, nil
}

// intN reads an n-byte big-endian TWO'S-COMPLEMENT integer as int64.
func (r *boltWireReader) intN(n int) (int64, error) {
	u, err := r.uN(n)
	if err != nil {
		return 0, err
	}
	switch n {
	case 1:
		return int64(int8(u)), nil
	case 2:
		return int64(int16(u)), nil
	case 4:
		return int64(int32(u)), nil
	default:
		return int64(u), nil
	}
}

// sizeN reads an n-byte big-endian UNSIGNED size prefix. PackStream sizes are
// unsigned, so a 32-bit size is read as uint32 and not as a negative int32.
func (r *boltWireReader) sizeN(n int) (int, error) {
	u, err := r.uN(n)
	if err != nil {
		return 0, err
	}
	if u > uint64(len(r.b)) {
		return 0, fmt.Errorf("packstream: declared size %d exceeds the %d-byte message", u, len(r.b))
	}
	return int(u), nil
}

// bytes reads n raw bytes, copied so the result does not alias the message.
func (r *boltWireReader) bytes(n int) ([]byte, error) {
	if r.i+n > len(r.b) {
		return nil, fmt.Errorf("packstream: truncated %d-byte blob at offset %d of %d", n, r.i, len(r.b))
	}
	out := make([]byte, n)
	copy(out, r.b[r.i:r.i+n])
	r.i += n
	return out, nil
}

// str reads an n-byte UTF-8 string.
func (r *boltWireReader) str(n int) (string, error) {
	p, err := r.bytes(n)
	if err != nil {
		return "", err
	}
	return string(p), nil
}

// list reads n values.
func (r *boltWireReader) list(n, depth int) ([]any, error) {
	out := make([]any, 0, n)
	for k := 0; k < n; k++ {
		v, err := r.value(depth + 1)
		if err != nil {
			return nil, fmt.Errorf("list element %d: %w", k, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// dict reads n string-keyed entries.
func (r *boltWireReader) dict(n, depth int) (map[string]any, error) {
	out := make(map[string]any, n)
	for k := 0; k < n; k++ {
		kv, err := r.value(depth + 1)
		if err != nil {
			return nil, fmt.Errorf("map key %d: %w", k, err)
		}
		key, ok := kv.(string)
		if !ok {
			return nil, fmt.Errorf("map key %d is %T, want a string", k, kv)
		}
		v, err := r.value(depth + 1)
		if err != nil {
			return nil, fmt.Errorf("map value for %q: %w", key, err)
		}
		out[key] = v
	}
	return out, nil
}

// structure reads a tag byte and n fields.
func (r *boltWireReader) structure(n, depth int) (boltWireStruct, error) {
	tag, err := r.u8()
	if err != nil {
		return boltWireStruct{}, err
	}
	fields, err := r.list(n, depth)
	if err != nil {
		return boltWireStruct{}, fmt.Errorf("struct 0x%02X: %w", tag, err)
	}
	return boltWireStruct{Tag: tag, Fields: fields}, nil
}

// boltWireCensus walks a decoded value and returns every structure's (tag, arity)
// in encounter order — outermost first, then its fields left to right. It is the
// quantity the layout clauses are stated over, because the marker's arity is
// exactly what a real driver's hydrator asserts.
func boltWireCensus(v any) []boltWireWidth {
	var out []boltWireWidth
	boltWireWalk(v, &out)
	return out
}

// boltWireWalk is boltWireCensus's recursion. Map values are walked in SORTED KEY
// order so the census is a function of the message and not of Go's map iteration
// order; without that the census would differ run to run and the determinism
// clause would be measuring the runtime rather than the server.
func boltWireWalk(v any, out *[]boltWireWidth) {
	switch t := v.(type) {
	case boltWireStruct:
		*out = append(*out, boltWireWidth{Tag: t.Tag, Arity: len(t.Fields)})
		for _, f := range t.Fields {
			boltWireWalk(f, out)
		}
	case packstream.Struct:
		*out = append(*out, boltWireWidth{Tag: t.Tag, Arity: len(t.Fields)})
		for _, f := range t.Fields {
			boltWireWalk(f, out)
		}
	case []any:
		// [packstream.Value] is a type ALIAS for any (bolt/packstream/value.go:40),
		// so this one arm also matches the []packstream.Value a decoded
		// [proto.Record] carries; a second case for it would not compile.
		for _, e := range t {
			boltWireWalk(e, out)
		}
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(t)) {
			boltWireWalk(t[k], out)
		}
	}
}

// boltWireRenderCensus renders a census as a stable one-line string.
func boltWireRenderCensus(c []boltWireWidth) string {
	if len(c) == 0 {
		return "<none>"
	}
	parts := make([]string, len(c))
	for i, w := range c {
		parts[i] = w.String()
	}
	return strings.Join(parts, " ")
}

// -----------------------------------------------------------------------------
// Evidence
// -----------------------------------------------------------------------------

// BoltVersionNegotiation is one raw-preamble probe: the four slots the client
// wrote and what the server answered.
//
// # Concurrency contract
//
// It is a plain value written by one goroutine during collection and read-only
// afterwards; it carries no synchronisation of its own.
type BoltVersionNegotiation struct {
	// Name identifies the case in a violation message.
	Name string
	// Slots renders the four offer slots as the client wrote them, so a failure
	// says what was actually sent rather than what the case was named.
	Slots string
	// Accepted is true when the server answered a non-zero version.
	Accepted bool
	// Got is the version the server answered (zero when it refused).
	Got proto.Version
	// ReadErr classifies a failure to read the 4-byte reply, empty when it
	// arrived. It is a CLASSIFICATION and not the raw error text, so the
	// rendering stays a function of the seed.
	ReadErr string
}

// BoltVersionAuthProbes records what each step of the authentication script drew
// at one negotiated version. Every field is a CLASSIFICATION (`SUCCESS`,
// `FAILURE`, `CLOSED`) plus, where a refusal is expected, the code and message,
// so nothing process-dependent reaches the rendering.
type BoltVersionAuthProbes struct {
	// HelloGoodKind / HelloGoodCode: a HELLO carrying the CORRECT credentials.
	HelloGoodKind string
	HelloGoodCode string
	// RunAfterHello*: a RUN sent immediately after that HELLO, with no LOGON.
	RunAfterHelloKind    string
	RunAfterHelloCode    string
	RunAfterHelloMessage string
	// HelloWrong*: a HELLO carrying a WRONG password, on its own connection.
	HelloWrongKind string
	HelloWrongCode string
	// WrongHelloConnAlive reports whether the connection survived that HELLO. At
	// the inline-auth versions a failed HELLO makes the session DEFUNCT and the
	// serve loop closes the socket (bolt/server/session.go:911), so the next write
	// fails; at the deferred-auth versions HELLO never authenticated, so it
	// cannot have refused and the connection is intact.
	WrongHelloConnAlive bool
	// HelloBareKind / HelloBareCode: a credential-LESS HELLO, on its own
	// connection.
	HelloBareKind string
	HelloBareCode string
	// LogonKind / RunAfterLogonKind: the LOGON that follows that bare HELLO where
	// the version allows one, and a RUN after it.
	LogonKind         string
	RunAfterLogonKind string
	// ResetAfterHelloKind / RunAfterReset*: a RESET sent on a session that has
	// completed HELLO but not LOGON, and the RUN that follows it.
	ResetAfterHelloKind  string
	RunAfterResetKind    string
	RunAfterResetCode    string
	RunAfterResetMessage string
}

// BoltVersionEntity is the wire shape of one graph entity captured at one
// version: the struct census of the whole RECORD plus the semantic core the
// checker compares ACROSS versions.
type BoltVersionEntity struct {
	// Census is every (tag, arity) in the record, in encounter order, as the
	// independent reader saw it.
	Census []boltWireWidth
	// CodecCensus is the same, as the module's own decoder saw it. The two must
	// agree; see `encoding-walker-agrees-with-codec`.
	CodecCensus []boltWireWidth
	// Bytes is the record's length on the wire. It is a coarse but completely
	// independent witness that the two versions did not emit the same message.
	Bytes int
	// NodeID / NodeLabels / NodeProps are the returned node's version-INVARIANT
	// core.
	NodeID     int64
	NodeLabels []string
	NodeProps  map[string]any
	// RelID / RelStart / RelEnd / RelType / RelProps are the returned
	// relationship's version-invariant core.
	RelID    int64
	RelStart int64
	RelEnd   int64
	RelType  string
	RelProps map[string]any
	// PathNodeIDs / PathRelIDs / PathIndices are the returned path's
	// version-invariant core.
	PathNodeIDs []int64
	PathRelIDs  []int64
	PathIndices []int64
	// ElementIDs are the trailing element_id strings, in encounter order: the
	// node's, then the relationship's three, then the path's. It is EMPTY at the
	// Bolt 4 layout, which is itself the assertion.
	ElementIDs []string
	// Err classifies a capture failure, empty on success.
	Err string
}

// BoltVersionTemporal is one temporal value captured at one version: the struct
// tag and its integer fields, plus the string field a named zone carries.
type BoltVersionTemporal struct {
	// Kind names the value (`date`, `datetime-offset`, …).
	Kind string
	// Tag is the PackStream structure signature.
	Tag byte
	// Ints are the structure's integer fields in order.
	Ints []int64
	// Zone is the trailing IANA zone name for a named-zone datetime, empty
	// otherwise.
	Zone string
	// Err classifies a capture failure, empty on success.
	Err string
}

// BoltVersionArm is everything one negotiated version produced.
type BoltVersionArm struct {
	// Name is the target's name (`4.4`).
	Name string
	// Asked is the version the arm set out to negotiate.
	Asked proto.Version
	// Canonical is what the CANONICAL spelling (exact version, slot 0, range 0)
	// negotiated, and Spelled what the SEED-CHOSEN spelling negotiated. Both must
	// equal Asked; recording them separately is what makes
	// `arm-spelling-invariant` a comparison rather than a restatement.
	Canonical proto.Version
	Spelled   proto.Version
	// Spelling renders the seed-chosen offer preamble, and SpellingDiffers
	// reports whether it differed from the canonical one at all — the non-vacuity
	// question for the invariance clause.
	Spelling        string
	SpellingDiffers bool
	// NegotiateErr classifies a handshake failure, empty on success.
	NegotiateErr string

	// Auth holds the authentication script's outcomes.
	Auth BoltVersionAuthProbes

	// Entity holds the entity-encoding capture.
	Entity BoltVersionEntity
	// Temporals holds one entry per temporal kind, in a fixed order.
	Temporals []BoltVersionTemporal

	// Params are the decoded round-trip values for each parameter kind, in the
	// fixed order of [boltVersionParamCases]. They are compared across versions
	// by value AND by concrete Go type, following rmp #2484: the dynamic type IS
	// the wire encoding, so an Integer silently replaced by the
	// identically-rendered Float must fail.
	Params []any
	// ParamErr classifies a round-trip failure, empty on success.
	ParamErr string

	// TxCommitted reports whether the arm's explicit BEGIN/RUN/COMMIT committed,
	// TxBookmark the bookmark its COMMIT returned (rendered positionally; see the
	// file comment), and TxCountAfter the number of marker nodes visible
	// immediately afterwards.
	TxCommitted  bool
	TxBookmark   string
	TxCountAfter int
	// TxErr classifies a transaction-script failure, empty on success.
	TxErr string

	// Abuse holds one entry per malformed/ill-timed probe, in the fixed order of
	// [boltVersionAbuseCases]: the code and message the server answered.
	Abuse []BoltVersionAbuse
}

// BoltVersionAbuse is one malformed or ill-timed message and the refusal it drew.
type BoltVersionAbuse struct {
	// Name identifies the probe.
	Name string
	// Kind classifies the reply (`FAILURE`, `SUCCESS`, `IGNORED`, `CLOSED`).
	Kind string
	// Code and Message are the FAILURE's fields, empty when there was none.
	Code    string
	Message string
}

// BoltVersionMatrixEvidence is everything one run of the version matrix
// observed. It is a pure record: the checkers below adjudicate it and it holds no
// verdict of its own.
//
// # Concurrency contract
//
// It is written by a single goroutine during collection and read-only afterwards;
// it carries no synchronisation and must not be shared with a running collector.
type BoltVersionMatrixEvidence struct {
	// Negotiations are the raw-preamble probes in the order they ran.
	Negotiations []BoltVersionNegotiation
	// Supported is what [proto.SupportedVersions] advertised at collection time,
	// compared against [boltVersionSupportedTripwire].
	Supported []proto.Version
	// Arms are the per-version arms in the order of [boltVersionTargets].
	Arms []BoltVersionArm
	// TemporalRef is the harness's OWN reference for every temporal value the
	// scenario asks for, computed with Go's time package from the same literals
	// the query carries. It is what makes the temporal clauses independent of the
	// encoder they adjudicate. NamedZoneResolved is false when the zone database
	// was unavailable, which the non-vacuity gate reports as a shortfall rather
	// than letting the named-zone clause pass unexercised.
	TemporalRef       BoltVersionTemporalRefs
	NamedZoneResolved bool
	// LiveMarkers and RecoveredMarkers are the marker nodes each version
	// committed, as counted in the live engine and in a graph reopened through
	// real WAL recovery after a crash. The maps are keyed by the version name the
	// client itself wrote into the node.
	LiveMarkers      map[string]int
	RecoveredMarkers map[string]int
	// Seed is the seed the run was built from.
	Seed uint64
}

// BoltVersionZoneRef is the harness-computed reference for one zoned instant.
type BoltVersionZoneRef struct {
	// UTCSeconds is the true UTC epoch second, Nanos the sub-second part, and
	// OffsetSeconds the zone's offset from UTC at that instant.
	UTCSeconds    int64
	Nanos         int64
	OffsetSeconds int64
}

// BoltVersionTemporalRefs is the harness's independently computed expectation for
// every temporal value the scenario asks the server to return. Each field is
// derived with Go's own time package from the SAME literal the query carries, so
// the temporal clauses compare the server's encoding against an outside
// computation rather than against the encoder that produced it.
type BoltVersionTemporalRefs struct {
	// EpochDay is date('2020-01-02') as whole days since the Unix epoch.
	EpochDay int64
	// NanosOfDay is '03:04:05.000000006' as nanoseconds since midnight, shared by
	// the localtime and time values.
	NanosOfDay int64
	// LocalAsUTCSec is the localdatetime's wall clock read as if it were UTC,
	// which is what a zone-less local datetime carries on the wire.
	LocalAsUTCSec int64
	// OffsetUTCSec / OffsetNanos / OffsetSeconds describe the offset-zone
	// datetime: the true UTC epoch second, the sub-second part, and the zone
	// offset.
	OffsetUTCSec  int64
	OffsetNanos   int64
	OffsetSeconds int64
	// Named describes the named-zone datetime the same way. It is meaningful only
	// when the zone database resolved; see
	// [BoltVersionMatrixEvidence.NamedZoneResolved].
	Named BoltVersionZoneRef
	// Duration is the duration's four fields in wire order.
	Duration []int64
}

// -----------------------------------------------------------------------------
// The scripts: what each arm sends
// -----------------------------------------------------------------------------

// The fixture and the queries every arm drives. They are built from the label and
// name constants so a rename cannot leave a query pointing at nothing.
const (
	boltVersionFixtureCreate = "CREATE (:" + boltVersionFixtureLabel + " {name:'" + boltVersionSrcName + "', n:1})" +
		"-[:" + boltVersionRelType + " {w:2.5}]->" +
		"(:" + boltVersionFixtureLabel + " {name:'" + boltVersionDstName + "'})"

	// boltVersionEntityQuery returns a node, a bound relationship and a path in
	// ONE record, so a single capture covers the 'N', 'R', 'P' and — inside the
	// path — 'r' structures, every one of which branches on the major version.
	boltVersionEntityQuery = "MATCH p=(a:" + boltVersionFixtureLabel + " {name:'" + boltVersionSrcName + "'})" +
		"-[e:" + boltVersionRelType + "]->" +
		"(b:" + boltVersionFixtureLabel + " {name:'" + boltVersionDstName + "'}) RETURN a, e, p"

	boltVersionMarkerCount = "MATCH (n:" + boltVersionMarkerLabel + ") RETURN count(n) AS c"
)

// The temporal literals. They are named constants because the harness reference
// below must be computed from the SAME instants the query asks for; two
// independent spellings of "the same" instant is how a temporal oracle silently
// stops testing anything.
const (
	boltVersionZoneName      = "Europe/Athens"
	boltVersionOffsetSeconds = int64(2 * 3600)
)

// boltVersionTemporalQuery asks for one value of every temporal kind the encoder
// handles, in the fixed order of [boltVersionTemporalKinds].
//
// The offset zone is +02:00 and the named zone is Europe/Athens on 2 January,
// where the zone is also +02:00. Both are DELIBERATELY non-zero: the Bolt 4
// legacy convention writes utcEpochSec + offsetSec where Bolt 5 writes
// utcEpochSec, so a zone at UTC+00:00 would make the two conventions produce the
// IDENTICAL seconds field and the clause could not tell them apart. Europe/Lisbon
// was the first zone tried and is exactly that trap — WET, offset 0, in January.
const boltVersionTemporalQuery = "RETURN " +
	"date('2020-01-02') AS d, " +
	"localtime('03:04:05.000000006') AS lt, " +
	"time('03:04:05.000000006+02:00') AS t, " +
	"localdatetime('2020-01-02T03:04:05.000000006') AS ldt, " +
	"datetime('2020-01-02T03:04:05.000000006+02:00') AS dto, " +
	"datetime({year:2020,month:1,day:2,hour:3,timezone:'" + boltVersionZoneName + "'}) AS dtn, " +
	"duration({months:1,days:2,seconds:3,nanoseconds:4}) AS dur"

// boltVersionTemporalKinds names the temporal columns in the order
// [boltVersionTemporalQuery] returns them.
var boltVersionTemporalKinds = []string{
	"date", "localtime", "time", "localdatetime", "datetime-offset", "datetime-named", "duration",
}

// boltVersionTemporalRefs computes the harness's own expectation for every
// temporal value, with Go's time package, from the literals in
// [boltVersionTemporalQuery].
//
// The zone database may be absent (a scratch container with no tzdata and no
// time/tzdata import). That is reported as a SHORTFALL by the non-vacuity gate
// rather than silently skipped: a named-zone clause that could not be constructed
// has not passed, it has not run.
func boltVersionComputeTemporalRefs() (BoltVersionTemporalRefs, bool) {
	offsetZone := time.FixedZone("+02:00", int(boltVersionOffsetSeconds))
	offsetInstant := time.Date(2020, time.January, 2, 3, 4, 5, 6, offsetZone)
	refs := BoltVersionTemporalRefs{
		EpochDay:      time.Date(2020, time.January, 2, 0, 0, 0, 0, time.UTC).Unix() / 86400,
		NanosOfDay:    ((3*3600)+(4*60)+5)*int64(time.Second) + 6,
		LocalAsUTCSec: time.Date(2020, time.January, 2, 3, 4, 5, 6, time.UTC).Unix(),
		OffsetUTCSec:  offsetInstant.Unix(),
		OffsetNanos:   int64(offsetInstant.Nanosecond()),
		OffsetSeconds: boltVersionOffsetSeconds,
		Duration:      []int64{1, 2, 3, 4},
	}
	loc, err := time.LoadLocation(boltVersionZoneName)
	if err != nil {
		return refs, false
	}
	named := time.Date(2020, time.January, 2, 3, 0, 0, 0, loc)
	_, off := named.Zone()
	refs.Named = BoltVersionZoneRef{
		UTCSeconds:    named.Unix(),
		Nanos:         int64(named.Nanosecond()),
		OffsetSeconds: int64(off),
	}
	return refs, true
}

// boltVersionParamCases is the parameter round-trip matrix every arm drives. The
// decoded value AND its concrete Go type must be identical at every version,
// following rmp #2484: the dynamic type IS the wire encoding, so an Integer
// silently re-encoded as the identically-rendered Float must fail rather than
// pass.
var boltVersionParamCases = []struct {
	Value any
	Name  string
}{
	{Name: "null", Value: nil},
	{Name: "bool", Value: true},
	{Name: "int", Value: int64(-1234567)},
	{Name: "float", Value: 2.5},
	{Name: "string", Value: "héllo"},
	{Name: "list", Value: []any{int64(1), "two", 3.0, nil, true}},
	{Name: "map", Value: map[string]any{"k": int64(7), "s": "v"}},
}

// boltVersionAbuseCases is the bad-actor battery every arm drives on its working
// connection, with a RESET between probes to clear the FAILED state each one
// leaves. The refusal must be IDENTICAL at every version: none of these three
// paths reads the negotiated version, so a version-dependent code or message here
// would be a genuine surprise.
//
// The expected code and message are LITERAL, so the clause pins the actual
// contract rather than only cross-version agreement — two versions could agree on
// a wrong answer.
var boltVersionAbuseCases = []struct {
	Name    string
	Code    string
	Message string
}{
	{
		// A correctly FRAMED chunk whose payload is a zero-field structure with an
		// unknown signature: the framing layer accepts it and the message layer
		// must reject it.
		Name: "garbage-opcode", Code: "Neo.ClientError.Request.Invalid", Message: "malformed Bolt message",
	},
	{
		Name: "commit-without-transaction", Code: "Neo.ClientError.Request.Invalid",
		Message: "illegal message *proto.Commit in state READY",
	},
	{
		Name: "pull-without-run", Code: "Neo.ClientError.Request.Invalid",
		Message: "illegal message *proto.Pull in state READY",
	},
}

// boltVersionGarbageOpcode is the payload of the garbage-opcode probe: a tiny
// struct header with zero fields (0xB0) and a signature byte the server assigns
// to no message (0x55).
var boltVersionGarbageOpcode = []byte{0xB0, 0x55}

// boltVersionNegotiationCase is one raw-preamble probe and the outcome this
// scenario requires of it. Want is meaningful only when Accept is true.
type boltVersionNegotiationCase struct {
	Name   string
	Slots  [4]BoltOffer
	Want   proto.Version
	Accept bool
}

// v is shorthand for a version in the case table below.
func v(major, minor uint8) proto.Version { return proto.Version{Major: major, Minor: minor} }

// boltVersionNegotiationCases is the negotiation table. Every expectation is a
// LITERAL that follows from [boltVersionSupportedTripwire]; the tripwire clause
// is what stops a change to the server's advertised list from turning these into
// statements about a server that no longer exists.
var boltVersionNegotiationCases = []boltVersionNegotiationCase{
	{Name: "exact-5.6", Slots: [4]BoltOffer{{Version: v(5, 6)}}, Accept: true, Want: v(5, 6)},
	{Name: "exact-5.1", Slots: [4]BoltOffer{{Version: v(5, 1)}}, Accept: true, Want: v(5, 1)},
	{Name: "exact-5.0", Slots: [4]BoltOffer{{Version: v(5, 0)}}, Accept: true, Want: v(5, 0)},
	{Name: "exact-4.4", Slots: [4]BoltOffer{{Version: v(4, 4)}}, Accept: true, Want: v(4, 4)},
	// A range offer resolves to its TOP when the top is supported.
	{Name: "range-5.6-down-to-5.0", Slots: [4]BoltOffer{{Version: v(5, 6), MinorRange: 6}}, Accept: true, Want: v(5, 6)},
	{Name: "range-5.2-down-to-5.0", Slots: [4]BoltOffer{{Version: v(5, 2), MinorRange: 2}}, Accept: true, Want: v(5, 2)},
	{Name: "range-4.4-down-to-4.0", Slots: [4]BoltOffer{{Version: v(4, 4), MinorRange: 4}}, Accept: true, Want: v(4, 4)},
	// A range whose top is ABOVE everything the server supports still resolves,
	// to the highest supported version inside it. That is the forward-compatible
	// path a future driver takes, and it lands on 5.6.
	{Name: "range-above-the-ceiling", Slots: [4]BoltOffer{{Version: v(5, 9), MinorRange: 9}}, Accept: true, Want: v(5, 6)},
	// Slot ORDER does not decide: the server's own preference does. The legacy
	// version is offered FIRST here and loses anyway.
	{
		Name:   "legacy-offered-first-loses",
		Slots:  [4]BoltOffer{{Version: v(4, 4)}, {Version: v(5, 6)}},
		Accept: true, Want: v(5, 6),
	},
	{
		Name:   "two-supported-picks-the-higher",
		Slots:  [4]BoltOffer{{Version: v(5, 0)}, {Version: v(4, 4)}},
		Accept: true, Want: v(5, 0),
	},
	// A decoy the server does not support, in a later slot, changes nothing.
	{
		Name:   "supported-with-unsupported-decoy",
		Slots:  [4]BoltOffer{{Version: v(4, 4)}, {}, {}, {Version: v(3, 0)}},
		Accept: true, Want: v(4, 4),
	},
	// Nothing in common is REFUSED, on each of the three ways to miss: a minor
	// below the only supported 4.x, a minor above the supported 5.x ceiling, and
	// a major on either side of the supported range.
	{Name: "unsupported-minor-4.3", Slots: [4]BoltOffer{{Version: v(4, 3)}}},
	{Name: "unsupported-minor-5.9", Slots: [4]BoltOffer{{Version: v(5, 9)}}},
	{Name: "unsupported-major-3.0", Slots: [4]BoltOffer{{Version: v(3, 0)}}},
	{Name: "unsupported-major-6.0", Slots: [4]BoltOffer{{Version: v(6, 0)}}},
}

// -----------------------------------------------------------------------------
// Collection
// -----------------------------------------------------------------------------

// boltVersionValidator is the handler the scenario's server authenticates
// through: exactly one principal and one password, compared in constant time.
// Never [server.NoAuthHandler] — see the file comment.
func boltVersionValidator() server.AuthHandler {
	return server.BasicAuthHandler{
		Validate: server.ConstantTimeValidate(boltVersionPrincipal, boltVersionPassword),
	}
}

// RunBoltVersionMatrix drives the whole version matrix once against a WAL-backed
// server whose AuthHandler validates credentials, and returns the evidence.
//
// It is bit-reproducible from seed: every arm is a fixed lock-step script on its
// own connections, the only seeded draws are the offer SPELLINGS (which must not
// change any outcome — that is the claim), and the only other seeded component is
// the SimDisk the WAL lives on.
//
// The returned error is reserved for HARNESS failures (the store would not open,
// a dial was refused, a record never arrived). A refused message, a rejected
// credential or an unexpected reply shape is EVIDENCE, not an error.
func RunBoltVersionMatrix(ctx context.Context, seed uint64) (*BoltVersionMatrixEvidence, error) {
	disk := NewSimDisk(NewSeed(seed^boltVersionSeedMix), 0) // faultRate 0: this scenario faults nothing
	cfg := durableStoreConfig()

	st, err := OpenSimStore(disk, cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-version-matrix open store: %w", err)
	}
	srv, err := NewSimServerAuth(st.Engine(), clock.Real(), boltVersionValidator())
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("sim: bolt-version-matrix server: %w", err)
	}

	refs, zoneOK := boltVersionComputeTemporalRefs()
	ev := &BoltVersionMatrixEvidence{
		Seed:              seed,
		Supported:         slices.Clone(proto.SupportedVersions),
		TemporalRef:       refs,
		NamedZoneResolved: zoneOK,
		LiveMarkers:       map[string]int{},
		RecoveredMarkers:  map[string]int{},
	}
	r := &boltVersionRunner{srv: srv, st: st, ev: ev, rng: NewSeed(seed)}

	if err := r.driveAll(ctx); err != nil {
		_ = srv.Close()
		st.Crash()
		return nil, err
	}
	if err := r.censusLive(ctx); err != nil {
		_ = srv.Close()
		st.Crash()
		return nil, err
	}

	// Crash (drop the engine, keep the SimDisk image) and reopen through real
	// recovery. A marker written over one protocol version but never made durable
	// is invisible to the live census and surfaces only here.
	_ = srv.Close()
	st.Crash()

	st2, err := OpenSimStore(disk, cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-version-matrix reopen: %w", err)
	}
	defer func() { _ = st2.Close() }()
	if err := r.censusRecovered(ctx, st2); err != nil {
		return nil, err
	}
	return ev, nil
}

// boltVersionRunner threads the server, the store, the seeded draw stream and the
// accumulating evidence through the arms.
type boltVersionRunner struct {
	srv *SimServer
	st  *SimStore
	ev  *BoltVersionMatrixEvidence
	rng *Seed
}

// driveAll runs the fixture, the raw negotiation table and then one arm per
// target version, in a fixed order.
//
// The fixture is created FIRST, over the harness's default handshake, so every
// arm reads a graph that already exists and none of them can be the one that
// created the entities it then describes. The negotiation table runs before the
// arms because an arm that cannot negotiate its version has nothing to say, and a
// table failure localises that to the handshake rather than to the arm.
func (r *boltVersionRunner) driveAll(ctx context.Context) error {
	if err := r.seedFixture(ctx); err != nil {
		return err
	}
	r.driveNegotiationTable()
	for i := range boltVersionTargets {
		if err := ctx.Err(); err != nil {
			return err
		}
		arm, err := r.driveArm(ctx, &boltVersionTargets[i])
		if err != nil {
			return err
		}
		r.ev.Arms = append(r.ev.Arms, arm)
	}
	return nil
}

// connectAt dials, negotiates exactly the given preamble, and returns the client
// with the handshake done but NO authentication attempted, so an auth probe owns
// every message after the preamble.
func (r *boltVersionRunner) connectAt(ctx context.Context, slots [4]BoltOffer) (*WireClient, proto.Version, error) {
	c, err := r.srv.Dial()
	if err != nil {
		return nil, proto.Version{}, fmt.Errorf("sim: bolt-version-matrix dial: %w", err)
	}
	got, err := c.HandshakeOfferingSlots(ctx, slots)
	if err != nil {
		_ = c.Close()
		return nil, proto.Version{}, err
	}
	return c, got, nil
}

// exactSlots is the CANONICAL spelling: the version alone, in slot 0, with no
// range and no companion.
func exactSlots(ver proto.Version) [4]BoltOffer { return [4]BoltOffer{{Version: ver}} }

// seedFixture creates the two-node, one-relationship graph every arm reads back.
func (r *boltVersionRunner) seedFixture(ctx context.Context) error {
	c, _, err := r.connectAt(ctx, exactSlots(proto.Version{Major: 5, Minor: 6}))
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if err := boltVersionAuthenticate(c); err != nil {
		return fmt.Errorf("sim: bolt-version-matrix fixture auth: %w", err)
	}
	if _, err := wireQuery(c, boltVersionFixtureCreate, nil); err != nil {
		return fmt.Errorf("sim: bolt-version-matrix fixture: %w", err)
	}
	return nil
}

// boltVersionAuthenticate takes the connection to a working session using the
// message the NEGOTIATED version puts the credentials on, and fails loudly rather
// than returning a half-authenticated client.
func boltVersionAuthenticate(c *WireClient) error {
	resp, err := c.AuthenticateAs(boltVersionPrincipal, boltVersionPassword)
	if err != nil {
		return err
	}
	if f, ok := resp.(*proto.Failure); ok {
		return fmt.Errorf("authentication refused: %s %s", f.Code, f.Message)
	}
	return nil
}

// -----------------------------------------------------------------------------
// The raw negotiation table
// -----------------------------------------------------------------------------

// driveNegotiationTable writes each case's 20-byte preamble on its own raw
// connection and records the 4 bytes that came back.
//
// It bypasses [WireClient] deliberately. HandshakeOfferingSlots collapses a
// rejection into an error, and telling a rejection apart from a transport failure
// by matching that error's TEXT would be a fragile oracle; reading the four reply
// bytes directly is the thing the clause is actually about.
func (r *boltVersionRunner) driveNegotiationTable() {
	for i := range boltVersionNegotiationCases {
		tc := &boltVersionNegotiationCases[i]
		obs := BoltVersionNegotiation{Name: tc.Name, Slots: boltVersionRenderSlots(tc.Slots)}
		conn, err := r.srv.DialConn()
		if err != nil {
			obs.ReadErr = "dial-refused"
			r.ev.Negotiations = append(r.ev.Negotiations, obs)
			continue
		}
		var buf [20]byte
		binary.BigEndian.PutUint32(buf[:4], proto.Magic)
		for slot, off := range tc.Slots {
			if off.Version == (proto.Version{}) {
				continue
			}
			k := 4 + slot*4
			buf[k], buf[k+1], buf[k+2], buf[k+3] = 0, off.MinorRange, off.Version.Minor, off.Version.Major
		}
		if _, err := conn.Write(buf[:]); err != nil {
			obs.ReadErr = "write-failed"
			_ = conn.Close()
			r.ev.Negotiations = append(r.ev.Negotiations, obs)
			continue
		}
		var resp [4]byte
		if _, err := readFullConn(conn, resp[:]); err != nil {
			obs.ReadErr = "reply-unreadable"
			_ = conn.Close()
			r.ev.Negotiations = append(r.ev.Negotiations, obs)
			continue
		}
		if resp[2] != 0 || resp[3] != 0 {
			obs.Accepted = true
			obs.Got = proto.Version{Major: resp[3], Minor: resp[2]}
		}
		_ = conn.Close()
		r.ev.Negotiations = append(r.ev.Negotiations, obs)
	}
}

// readFullConn fills p from conn, looping over short reads.
func readFullConn(conn *SimConn, p []byte) (int, error) {
	total := 0
	for total < len(p) {
		n, err := conn.Read(p[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// boltVersionRenderSlots renders a preamble's four slots as a stable string, so a
// violation reports what was actually sent rather than only the case's name.
func boltVersionRenderSlots(slots [4]BoltOffer) string {
	parts := make([]string, 0, 4)
	for _, o := range slots {
		if o.Version == (proto.Version{}) {
			parts = append(parts, "-")
			continue
		}
		if o.MinorRange == 0 {
			parts = append(parts, fmt.Sprintf("%d.%d", o.Version.Major, o.Version.Minor))
			continue
		}
		parts = append(parts, fmt.Sprintf("%d.%d~%d", o.Version.Major, o.Version.Minor, o.MinorRange))
	}
	return strings.Join(parts, ",")
}

// -----------------------------------------------------------------------------
// One arm
// -----------------------------------------------------------------------------

// driveArm runs the whole script for one target version: the canonical and the
// seed-chosen spellings of the handshake, the four authentication probes, and
// then — on a working connection at the seed-chosen spelling — the entity,
// temporal, parameter, transaction and abuse captures.
func (r *boltVersionRunner) driveArm(ctx context.Context, t *boltVersionTarget) (BoltVersionArm, error) {
	arm := BoltVersionArm{Name: t.Name, Asked: t.Version}

	// The canonical spelling first, on the connection the good-credentials probe
	// then uses: negotiating twice on one connection is not a second handshake, it
	// is 20 bytes of garbage arriving where the server expects a chunked message.
	c, got, err := r.connectAt(ctx, exactSlots(t.Version))
	if err != nil {
		arm.NegotiateErr = "canonical-rejected"
		return arm, nil // a refused negotiation is EVIDENCE; the clause below adjudicates it.
	}
	arm.Canonical = got
	r.probeGoodCredentials(c, &arm)
	_ = c.Close()

	if err := r.probeWrongCredentials(ctx, t, &arm); err != nil {
		return arm, err
	}
	if err := r.probeBareHello(ctx, t, &arm); err != nil {
		return arm, err
	}
	if err := r.probeResetBeforeLogon(ctx, t, &arm); err != nil {
		return arm, err
	}

	// The seed-chosen spelling drives the working connection, so the invariance
	// claim is not merely that the two spellings negotiate the same version but
	// that every observable below was produced over the seeded one.
	slots, rendered, differs := r.drawSpelling(t)
	arm.Spelling, arm.SpellingDiffers = rendered, differs
	work, spelled, err := r.connectAt(ctx, slots)
	if err != nil {
		arm.NegotiateErr = "spelled-rejected"
		return arm, nil // a refused negotiation is EVIDENCE; the clause below adjudicates it.
	}
	arm.Spelled = spelled
	defer func() { _ = work.Close() }()
	if err := boltVersionAuthenticate(work); err != nil {
		return arm, fmt.Errorf("sim: bolt-version-matrix %s work auth: %w", t.Name, err)
	}

	arm.Entity = r.captureEntities(work)
	arm.Temporals = r.captureTemporals(work)
	r.captureParams(work, &arm)
	r.captureTransaction(work, t, &arm)
	r.captureAbuse(work, &arm)
	return arm, nil
}

// drawSpelling draws the seed-chosen offer preamble for one target: the slot the
// target sits in, the minor range below it, and whether an unsupported decoy
// shares the preamble.
//
// Every draw is a spelling of the SAME offer, so the negotiated version must not
// move. The draws are bounded by the target's own MaxMinorRange, because a range
// wider than the minor would underflow — Negotiate guards that
// (bolt/proto/handshake.go:116-120) — and asking for a spelling that cannot
// resolve to the target would be probing rejection, not invariance.
func (r *boltVersionRunner) drawSpelling(t *boltVersionTarget) (slots [4]BoltOffer, rendered string, differs bool) {
	slot := int(r.rng.Uint64N(4))
	minorRange := uint8(0)
	if t.MaxMinorRange > 0 {
		minorRange = uint8(r.rng.Uint64N(uint64(t.MaxMinorRange) + 1))
	}
	withDecoy := r.rng.Uint64N(2) == 1
	decoyPick := int(r.rng.Uint64N(uint64(len(boltVersionDecoys))))

	slots[slot] = BoltOffer{Version: t.Version, MinorRange: minorRange}
	if withDecoy {
		// Place the decoy in the first slot that is not the target's, so the two
		// offers cannot collide and overwrite each other.
		for k := 0; k < 4; k++ {
			if k != slot {
				slots[k] = BoltOffer{Version: boltVersionDecoys[decoyPick]}
				break
			}
		}
	}
	rendered = boltVersionRenderSlots(slots)
	return slots, rendered, rendered != boltVersionRenderSlots(exactSlots(t.Version))
}

// boltVersionDecoys are versions this server does not support, offered alongside
// a supported one to show that an unsupported companion changes nothing.
var boltVersionDecoys = []proto.Version{{Major: 3, Minor: 0}, {Major: 4, Minor: 3}, {Major: 6, Minor: 0}}

// -----------------------------------------------------------------------------
// The authentication probes
// -----------------------------------------------------------------------------

// boltVersionBasicToken is the auth map a driver sends for the basic scheme,
// with the user agent HELLO also carries.
func boltVersionBasicToken(credentials string) map[string]packstream.Value {
	return map[string]packstream.Value{
		"scheme":      "basic",
		"principal":   boltVersionPrincipal,
		"credentials": credentials,
		"user_agent":  "gograph-sim-version-matrix/1.0",
	}
}

// boltVersionBareHello is a HELLO carrying driver metadata and NO credentials —
// what a Bolt 5.1+ driver actually sends.
func boltVersionBareHello() map[string]packstream.Value {
	return map[string]packstream.Value{"user_agent": "gograph-sim-version-matrix/1.0"}
}

// probeGoodCredentials sends a HELLO with the CORRECT password and then a RUN
// with no LOGON in between. Whether that RUN is served is the whole no-LOGON
// contract, stated on the state machine's behaviour rather than on a reply code
// alone.
func (r *boltVersionRunner) probeGoodCredentials(c *WireClient, arm *BoltVersionArm) {
	resp, err := c.Hello(boltVersionBasicToken(boltVersionPassword))
	arm.Auth.HelloGoodKind, arm.Auth.HelloGoodCode, _ = boltVersionClassify(resp, err)
	run, err := c.Run("RETURN 1", nil)
	arm.Auth.RunAfterHelloKind, arm.Auth.RunAfterHelloCode, arm.Auth.RunAfterHelloMessage = boltVersionClassify(run, err)
	if arm.Auth.RunAfterHelloKind == boltVersionKindSuccess {
		// Drain the stream so the connection is not abandoned mid-result; the
		// outcome of the drain is not part of any clause.
		_, _, _ = c.PullAll()
	}
}

// probeWrongCredentials sends a HELLO with a WRONG password on its own
// connection and then measures whether the connection SURVIVED.
//
// Liveness is measured by sending a RESET, which is legal from every non-DEFUNCT
// state, so a failure to complete it is a closed socket rather than a refused
// message. At the inline-auth versions the failed HELLO makes the session DEFUNCT
// and the serve loop closes the socket; at the deferred-auth versions HELLO never
// authenticated, so there was nothing to refuse.
func (r *boltVersionRunner) probeWrongCredentials(ctx context.Context, t *boltVersionTarget, arm *BoltVersionArm) error {
	c, _, err := r.connectAt(ctx, exactSlots(t.Version))
	if err != nil {
		return fmt.Errorf("sim: bolt-version-matrix %s wrong-creds negotiate: %w", t.Name, err)
	}
	defer func() { _ = c.Close() }()
	resp, err := c.Hello(boltVersionBasicToken("not-the-password"))
	arm.Auth.HelloWrongKind, arm.Auth.HelloWrongCode, _ = boltVersionClassify(resp, err)
	probe, err := c.Reset()
	kind, _, _ := boltVersionClassify(probe, err)
	arm.Auth.WrongHelloConnAlive = kind != boltVersionKindClosed
	return nil
}

// probeBareHello sends a credential-LESS HELLO, then the LOGON that a 5.1+ driver
// follows it with, then a RUN. At the inline-auth versions the bare HELLO is a
// credential rejection and everything after it lands on a closed socket, which is
// itself the assertion.
func (r *boltVersionRunner) probeBareHello(ctx context.Context, t *boltVersionTarget, arm *BoltVersionArm) error {
	c, _, err := r.connectAt(ctx, exactSlots(t.Version))
	if err != nil {
		return fmt.Errorf("sim: bolt-version-matrix %s bare-hello negotiate: %w", t.Name, err)
	}
	defer func() { _ = c.Close() }()
	resp, err := c.Hello(boltVersionBareHello())
	arm.Auth.HelloBareKind, arm.Auth.HelloBareCode, _ = boltVersionClassify(resp, err)

	logon, err := c.LogonWith(map[string]packstream.Value{
		"scheme": "basic", "principal": boltVersionPrincipal, "credentials": boltVersionPassword,
	})
	arm.Auth.LogonKind, _, _ = boltVersionClassify(logon, err)

	run, err := c.Run("RETURN 1", nil)
	arm.Auth.RunAfterLogonKind, _, _ = boltVersionClassify(run, err)
	if arm.Auth.RunAfterLogonKind == boltVersionKindSuccess {
		_, _, _ = c.PullAll()
	}
	return nil
}

// probeResetBeforeLogon sends a successful HELLO and then a RESET, and measures
// what the session can do afterwards.
//
// This is the pre-authentication RESET gate of task #1345: RESET must never grant
// access that authentication has not, so a connection that is not yet
// authenticated returns to NEGOTIATION rather than to READY
// (Session.handleReset's !s.authenticated branch, bolt/server/session.go:1038-1041,
// backed by the state machine at bolt/server/state.go:124-133). At 4.4 and 5.0 the
// HELLO already authenticated, so RESET reaches READY and the session keeps
// working; at 5.1 and 5.6 it has not, so the following RUN is refused naming
// NEGOTIATION.
func (r *boltVersionRunner) probeResetBeforeLogon(ctx context.Context, t *boltVersionTarget, arm *BoltVersionArm) error {
	c, _, err := r.connectAt(ctx, exactSlots(t.Version))
	if err != nil {
		return fmt.Errorf("sim: bolt-version-matrix %s reset negotiate: %w", t.Name, err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Hello(boltVersionBasicToken(boltVersionPassword)); err != nil {
		return fmt.Errorf("sim: bolt-version-matrix %s reset hello: %w", t.Name, err)
	}
	resp, err := c.Reset()
	arm.Auth.ResetAfterHelloKind, _, _ = boltVersionClassify(resp, err)
	run, err := c.Run("RETURN 1", nil)
	arm.Auth.RunAfterResetKind, arm.Auth.RunAfterResetCode, arm.Auth.RunAfterResetMessage = boltVersionClassify(run, err)
	if arm.Auth.RunAfterResetKind == boltVersionKindSuccess {
		_, _, _ = c.PullAll()
	}
	return nil
}

// The reply classifications. They are a small closed vocabulary rather than raw
// error text, so nothing machine-dependent reaches the rendering the determinism
// clause compares.
const (
	boltVersionKindSuccess = "SUCCESS"
	boltVersionKindFailure = "FAILURE"
	boltVersionKindIgnored = "IGNORED"
	boltVersionKindClosed  = "CLOSED"
	boltVersionKindOther   = "OTHER"
)

// boltVersionClassify reduces a reply (or a transport error) to a kind, a failure
// code and a failure message. A transport error is CLOSED: on this in-memory
// listener the only way a well-framed request fails to complete is that the
// server tore the connection down.
func boltVersionClassify(resp any, err error) (kind, code, message string) {
	if err != nil {
		return boltVersionKindClosed, "", ""
	}
	switch t := resp.(type) {
	case *proto.Success:
		return boltVersionKindSuccess, "", ""
	case *proto.Failure:
		return boltVersionKindFailure, t.Code, t.Message
	case *proto.Ignored:
		return boltVersionKindIgnored, "", ""
	default:
		return boltVersionKindOther, "", ""
	}
}

// -----------------------------------------------------------------------------
// The wire captures
// -----------------------------------------------------------------------------

// boltVersionCaptureRecord runs one query and returns the RAW bytes of its first
// RECORD together with the same record as the module's own decoder read it.
//
// Returning both is what makes `encoding-walker-agrees-with-codec` possible: the
// independent reader and the module codec are handed the identical bytes, so a
// bug in either surfaces as a disagreement rather than as a confident wrong
// verdict. It reads to the terminal reply so the session is left in READY.
func boltVersionCaptureRecord(c *WireClient, query string) (raw []byte, codec []packstream.Value, err error) {
	resp, err := c.Run(query, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("RUN: %w", err)
	}
	if f, ok := resp.(*proto.Failure); ok {
		return nil, nil, fmt.Errorf("RUN refused: %s %s", f.Code, f.Message)
	}
	if err := c.send(&proto.Pull{N: -1, QID: -1}); err != nil {
		return nil, nil, fmt.Errorf("PULL: %w", err)
	}
	for {
		msg, err := c.RecvRaw()
		if err != nil {
			return nil, nil, fmt.Errorf("read: %w", err)
		}
		decoded, err := proto.DecodeResponse(packstream.NewDecoder(bytes.NewReader(msg)))
		if err != nil {
			return nil, nil, fmt.Errorf("decode: %w", err)
		}
		switch t := decoded.(type) {
		case *proto.Record:
			if raw == nil {
				raw = slices.Clone(msg)
				codec = t.Data
			}
		case *proto.Failure:
			return nil, nil, fmt.Errorf("PULL refused: %s %s", t.Code, t.Message)
		default:
			if raw == nil {
				return nil, nil, fmt.Errorf("query returned no record")
			}
			return raw, codec, nil
		}
	}
}

// boltVersionRecordColumns decodes a raw RECORD message with the INDEPENDENT
// reader and returns its column list. A RECORD is the one-field structure 0x71
// whose field is the list of columns.
func boltVersionRecordColumns(raw []byte) ([]any, error) {
	v, err := decodeBoltWire(raw)
	if err != nil {
		return nil, err
	}
	st, ok := v.(boltWireStruct)
	if !ok {
		return nil, fmt.Errorf("message is %T, want a structure", v)
	}
	if st.Tag != boltTagRecord || len(st.Fields) != 1 {
		return nil, fmt.Errorf("message is structure 0x%02X with %d field(s), want RECORD 0x%02X with 1", st.Tag, len(st.Fields), boltTagRecord)
	}
	cols, ok := st.Fields[0].([]any)
	if !ok {
		return nil, fmt.Errorf("RECORD field is %T, want a list", st.Fields[0])
	}
	return cols, nil
}

// captureEntities drives the entity query and extracts both the struct census and
// the version-invariant semantic core of the node, the relationship and the path.
//
// The core is extracted from the LEADING fields regardless of the structure's
// arity, and the element_ids from whatever follows them. That ordering matters:
// reading the core positionally from the front cannot be biased by what the arm
// EXPECTS the arity to be, so `invariant-entity-core` and `encoding-node-arity`
// stay independent clauses rather than two spellings of one assumption.
func (r *boltVersionRunner) captureEntities(c *WireClient) BoltVersionEntity {
	var out BoltVersionEntity
	raw, codec, err := boltVersionCaptureRecord(c, boltVersionEntityQuery)
	if err != nil {
		out.Err = "capture: " + err.Error()
		return out
	}
	cols, err := boltVersionRecordColumns(raw)
	if err != nil {
		out.Err = "independent decode: " + err.Error()
		return out
	}
	out.Bytes = len(raw)
	out.Census = boltWireCensus(cols)
	out.CodecCensus = boltWireCensus([]any(codec))
	if len(cols) != 3 {
		out.Err = fmt.Sprintf("record has %d columns, want 3 (node, relationship, path)", len(cols))
		return out
	}
	if err := boltVersionFillEntity(&out, cols); err != nil {
		out.Err = err.Error()
	}
	return out
}

// boltVersionFillEntity extracts the three columns into the evidence.
func boltVersionFillEntity(out *BoltVersionEntity, cols []any) error {
	node, err := boltWireAsStruct(cols[0], boltTagNode, 3)
	if err != nil {
		return fmt.Errorf("column 0 (node): %w", err)
	}
	if out.NodeID, out.NodeLabels, out.NodeProps, err = boltVersionNodeCore(node); err != nil {
		return fmt.Errorf("node core: %w", err)
	}
	ids, err := boltWireTailStrings(node, 3)
	if err != nil {
		return fmt.Errorf("node element ids: %w", err)
	}
	out.ElementIDs = append(out.ElementIDs, ids...)

	rel, err := boltWireAsStruct(cols[1], boltTagRelationship, 5)
	if err != nil {
		return fmt.Errorf("column 1 (relationship): %w", err)
	}
	if out.RelID, err = boltWireInt(rel.Fields[0]); err != nil {
		return fmt.Errorf("relationship id: %w", err)
	}
	if out.RelStart, err = boltWireInt(rel.Fields[1]); err != nil {
		return fmt.Errorf("relationship start: %w", err)
	}
	if out.RelEnd, err = boltWireInt(rel.Fields[2]); err != nil {
		return fmt.Errorf("relationship end: %w", err)
	}
	if out.RelType, err = boltWireStr(rel.Fields[3]); err != nil {
		return fmt.Errorf("relationship type: %w", err)
	}
	if out.RelProps, err = boltWireMap(rel.Fields[4]); err != nil {
		return fmt.Errorf("relationship properties: %w", err)
	}
	if ids, err = boltWireTailStrings(rel, 5); err != nil {
		return fmt.Errorf("relationship element ids: %w", err)
	}
	out.ElementIDs = append(out.ElementIDs, ids...)

	return boltVersionFillPath(out, cols[2])
}

// boltVersionFillPath extracts the path column: its node ids, its unbound
// relationship ids, its index pairs, and the element_ids its members carry.
func boltVersionFillPath(out *BoltVersionEntity, col any) error {
	path, err := boltWireAsStruct(col, boltTagPath, 3)
	if err != nil {
		return fmt.Errorf("column 2 (path): %w", err)
	}
	nodes, ok := path.Fields[0].([]any)
	if !ok {
		return fmt.Errorf("path node list is %T, want a list", path.Fields[0])
	}
	for i, n := range nodes {
		st, err := boltWireAsStruct(n, boltTagNode, 3)
		if err != nil {
			return fmt.Errorf("path node %d: %w", i, err)
		}
		id, err := boltWireInt(st.Fields[0])
		if err != nil {
			return fmt.Errorf("path node %d id: %w", i, err)
		}
		out.PathNodeIDs = append(out.PathNodeIDs, id)
		ids, err := boltWireTailStrings(st, 3)
		if err != nil {
			return fmt.Errorf("path node %d element id: %w", i, err)
		}
		out.ElementIDs = append(out.ElementIDs, ids...)
	}
	rels, ok := path.Fields[1].([]any)
	if !ok {
		return fmt.Errorf("path relationship list is %T, want a list", path.Fields[1])
	}
	for i, rl := range rels {
		st, err := boltWireAsStruct(rl, boltTagUnboundRelationship, 3)
		if err != nil {
			return fmt.Errorf("path relationship %d: %w", i, err)
		}
		id, err := boltWireInt(st.Fields[0])
		if err != nil {
			return fmt.Errorf("path relationship %d id: %w", i, err)
		}
		out.PathRelIDs = append(out.PathRelIDs, id)
		ids, err := boltWireTailStrings(st, 3)
		if err != nil {
			return fmt.Errorf("path relationship %d element id: %w", i, err)
		}
		out.ElementIDs = append(out.ElementIDs, ids...)
	}
	idx, err := boltWireInts(path.Fields[2])
	if err != nil {
		return fmt.Errorf("path indices: %w", err)
	}
	out.PathIndices = idx
	return nil
}

// boltVersionNodeCore reads the three leading fields every 'N' structure carries.
func boltVersionNodeCore(st boltWireStruct) (id int64, labels []string, props map[string]any, err error) {
	if id, err = boltWireInt(st.Fields[0]); err != nil {
		return 0, nil, nil, fmt.Errorf("id: %w", err)
	}
	if labels, err = boltWireStrings(st.Fields[1]); err != nil {
		return 0, nil, nil, fmt.Errorf("labels: %w", err)
	}
	if props, err = boltWireMap(st.Fields[2]); err != nil {
		return 0, nil, nil, fmt.Errorf("properties: %w", err)
	}
	return id, labels, props, nil
}

// The independent reader's small typed accessors. Each is a loud error rather
// than a zero value, so a mis-shaped capture fails at the field that was wrong.

func boltWireAsStruct(v any, wantTag byte, minFields int) (boltWireStruct, error) {
	st, ok := v.(boltWireStruct)
	if !ok {
		return boltWireStruct{}, fmt.Errorf("value is %T, want a structure", v)
	}
	if st.Tag != wantTag {
		return boltWireStruct{}, fmt.Errorf("structure tag is 0x%02X, want 0x%02X", st.Tag, wantTag)
	}
	if len(st.Fields) < minFields {
		return boltWireStruct{}, fmt.Errorf("structure 0x%02X has %d field(s), want at least %d", st.Tag, len(st.Fields), minFields)
	}
	return st, nil
}

func boltWireTailStrings(st boltWireStruct, after int) ([]string, error) {
	out := make([]string, 0, len(st.Fields)-after)
	for i := after; i < len(st.Fields); i++ {
		s, err := boltWireStr(st.Fields[i])
		if err != nil {
			return nil, fmt.Errorf("trailing field %d: %w", i, err)
		}
		out = append(out, s)
	}
	return out, nil
}

func boltWireInt(v any) (int64, error) {
	n, ok := v.(int64)
	if !ok {
		return 0, fmt.Errorf("value is %T, want an integer", v)
	}
	return n, nil
}

func boltWireStr(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("value is %T, want a string", v)
	}
	return s, nil
}

func boltWireMap(v any) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("value is %T, want a map", v)
	}
	return m, nil
}

func boltWireStrings(v any) ([]string, error) {
	lst, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("value is %T, want a list", v)
	}
	out := make([]string, 0, len(lst))
	for i, e := range lst {
		s, err := boltWireStr(e)
		if err != nil {
			return nil, fmt.Errorf("element %d: %w", i, err)
		}
		out = append(out, s)
	}
	return out, nil
}

func boltWireInts(v any) ([]int64, error) {
	lst, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("value is %T, want a list", v)
	}
	out := make([]int64, 0, len(lst))
	for i, e := range lst {
		n, err := boltWireInt(e)
		if err != nil {
			return nil, fmt.Errorf("element %d: %w", i, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// captureTemporals drives the temporal query and records each column's structure
// tag, its integer fields and — for the named-zone datetime — its zone string.
func (r *boltVersionRunner) captureTemporals(c *WireClient) []BoltVersionTemporal {
	out := make([]BoltVersionTemporal, len(boltVersionTemporalKinds))
	for i, kind := range boltVersionTemporalKinds {
		out[i] = BoltVersionTemporal{Kind: kind}
	}
	raw, _, err := boltVersionCaptureRecord(c, boltVersionTemporalQuery)
	if err != nil {
		for i := range out {
			out[i].Err = "capture: " + err.Error()
		}
		return out
	}
	cols, err := boltVersionRecordColumns(raw)
	if err != nil {
		for i := range out {
			out[i].Err = "independent decode: " + err.Error()
		}
		return out
	}
	if len(cols) != len(out) {
		for i := range out {
			out[i].Err = fmt.Sprintf("record has %d columns, want %d", len(cols), len(out))
		}
		return out
	}
	for i := range out {
		st, ok := cols[i].(boltWireStruct)
		if !ok {
			out[i].Err = fmt.Sprintf("column is %T, want a structure", cols[i])
			continue
		}
		out[i].Tag = st.Tag
		for _, f := range st.Fields {
			switch fv := f.(type) {
			case int64:
				out[i].Ints = append(out[i].Ints, fv)
			case string:
				out[i].Zone = fv
			default:
				out[i].Err = fmt.Sprintf("field is %T, want an integer or a zone name", f)
			}
		}
	}
	return out
}

// captureParams round-trips every parameter kind and records the DECODED value,
// concrete Go type included.
func (r *boltVersionRunner) captureParams(c *WireClient, arm *BoltVersionArm) {
	arm.Params = make([]any, len(boltVersionParamCases))
	for i, tc := range boltVersionParamCases {
		recs, err := wireQuery(c, "RETURN $p AS v", map[string]any{"p": tc.Value})
		if err != nil {
			arm.ParamErr = fmt.Sprintf("%s: %v", tc.Name, err)
			return
		}
		if len(recs) != 1 || len(recs[0].Data) != 1 {
			arm.ParamErr = fmt.Sprintf("%s: got %d record(s), want 1 with 1 column", tc.Name, len(recs))
			return
		}
		arm.Params[i] = recs[0].Data[0]
	}
}

// captureTransaction drives an explicit BEGIN / RUN / PULL / COMMIT that writes
// this arm's marker node, and records the bookmark the COMMIT returned and the
// marker count immediately afterwards.
func (r *boltVersionRunner) captureTransaction(c *WireClient, t *boltVersionTarget, arm *BoltVersionArm) {
	begin, err := c.Begin()
	if kind, _, _ := boltVersionClassify(begin, err); kind != boltVersionKindSuccess {
		arm.TxErr = "BEGIN: " + kind
		return
	}
	create := fmt.Sprintf("CREATE (:%s {%s:'%s'})", boltVersionMarkerLabel, boltVersionMarkerKey, t.Name)
	if _, err := wireQuery(c, create, nil); err != nil {
		arm.TxErr = "CREATE: " + err.Error()
		return
	}
	commit, err := c.Commit()
	kind, _, _ := boltVersionClassify(commit, err)
	if kind != boltVersionKindSuccess {
		arm.TxErr = "COMMIT: " + kind
		return
	}
	arm.TxCommitted = true
	if s, ok := commit.(*proto.Success); ok {
		if bm, ok := s.Metadata["bookmark"].(string); ok {
			arm.TxBookmark = bm
		}
	}
	n, err := boltVersionCountViaWire(c, boltVersionMarkerCount)
	if err != nil {
		arm.TxErr = "count: " + err.Error()
		return
	}
	arm.TxCountAfter = n
}

// boltVersionCountViaWire reads a single-column count over the wire.
func boltVersionCountViaWire(c *WireClient, query string) (int, error) {
	recs, err := wireQuery(c, query, nil)
	if err != nil {
		return 0, err
	}
	if len(recs) != 1 || len(recs[0].Data) != 1 {
		return 0, fmt.Errorf("got %d record(s), want 1 with 1 column", len(recs))
	}
	n, ok := recs[0].Data[0].(int64)
	if !ok {
		return 0, fmt.Errorf("count column is %T, want an integer", recs[0].Data[0])
	}
	return int(n), nil
}

// captureAbuse drives the bad-actor battery on the working connection, RESETting
// between probes because each refusal leaves the session FAILED.
//
// The RESET reaches READY here rather than NEGOTIATION: this connection has
// completed authentication for its version, so [Session.handleReset]'s
// !s.authenticated branch does not apply. That is exactly the difference
// `logon-reset-before-logon` measures on its own connection.
func (r *boltVersionRunner) captureAbuse(c *WireClient, arm *BoltVersionArm) {
	arm.Abuse = make([]BoltVersionAbuse, len(boltVersionAbuseCases))
	for i, tc := range boltVersionAbuseCases {
		arm.Abuse[i].Name = tc.Name
		var resp any
		var err error
		switch tc.Name {
		case "garbage-opcode":
			if werr := c.WriteChunkedRaw(boltVersionGarbageOpcode); werr != nil {
				arm.Abuse[i].Kind = boltVersionKindClosed
				continue
			}
			resp, err = c.Recv()
		case "commit-without-transaction":
			resp, err = c.Commit()
		case "pull-without-run":
			_, resp, err = c.PullAll()
		default:
			// A roster entry with no driver here would otherwise silently reuse
			// another probe's message and report its refusal as its own.
			arm.Abuse[i].Kind = boltVersionKindOther
			arm.Abuse[i].Message = "no driver for this probe in captureAbuse"
			continue
		}
		arm.Abuse[i].Kind, arm.Abuse[i].Code, arm.Abuse[i].Message = boltVersionClassify(resp, err)
		if _, rerr := c.Reset(); rerr != nil {
			// A connection that cannot be RESET cannot carry the next probe; the
			// remaining entries stay CLOSED, which the clause reports.
			for k := i + 1; k < len(arm.Abuse); k++ {
				arm.Abuse[k].Name, arm.Abuse[k].Kind = boltVersionAbuseCases[k].Name, boltVersionKindClosed
			}
			return
		}
	}
}

// censusLive counts each arm's marker node in the LIVE engine, over the wire,
// before anything is torn down.
func (r *boltVersionRunner) censusLive(ctx context.Context) error {
	c, _, err := r.connectAt(ctx, exactSlots(proto.Version{Major: 5, Minor: 6}))
	if err != nil {
		return fmt.Errorf("sim: bolt-version-matrix census dial: %w", err)
	}
	defer func() { _ = c.Close() }()
	if err := boltVersionAuthenticate(c); err != nil {
		return fmt.Errorf("sim: bolt-version-matrix census auth: %w", err)
	}
	for i := range boltVersionTargets {
		name := boltVersionTargets[i].Name
		q := fmt.Sprintf("MATCH (n:%s {%s:'%s'}) RETURN count(n) AS c",
			boltVersionMarkerLabel, boltVersionMarkerKey, name)
		n, err := boltVersionCountViaWire(c, q)
		if err != nil {
			return fmt.Errorf("sim: bolt-version-matrix live census %s: %w", name, err)
		}
		r.ev.LiveMarkers[name] = n
	}
	return nil
}

// censusRecovered repeats the census against a graph reopened through REAL WAL
// recovery, so a marker that reached the engine but never the log is caught.
func (r *boltVersionRunner) censusRecovered(ctx context.Context, st *SimStore) error {
	for i := range boltVersionTargets {
		name := boltVersionTargets[i].Name
		q := fmt.Sprintf("MATCH (n:%s {%s:'%s'}) RETURN count(n)",
			boltVersionMarkerLabel, boltVersionMarkerKey, name)
		n, err := scalarCountViaEngine(ctx, st.Engine(), q)
		if err != nil {
			return fmt.Errorf("sim: bolt-version-matrix recovered census %s: %w", name, err)
		}
		r.ev.RecoveredMarkers[name] = int(n)
	}
	return nil
}

// -----------------------------------------------------------------------------
// The contract
// -----------------------------------------------------------------------------

// boltVersionOp names a clause in a violation.
func boltVersionOp(clause string) string { return "<bolt-version-matrix:" + clause + ">" }

// boltVersionViolation builds a violation for one clause.
func boltVersionViolation(kind ViolationKind, clause, msg string) Violation {
	return Violation{Kind: kind, Op: boltVersionOp(clause), Message: msg}
}

// checkBoltVersionMatrix adjudicates the evidence against the contract. It
// answers "did the server behave correctly"; whether the run was in a POSITION to
// notice is the separate question [checkBoltVersionMatrixNonVacuity] answers.
func checkBoltVersionMatrix(e *BoltVersionMatrixEvidence) []Violation {
	return slices.Concat(
		checkBoltVersionNegotiation(e),
		checkBoltVersionLogonContract(e),
		checkBoltVersionEncodings(e),
		checkBoltVersionInvariance(e),
		checkBoltVersionTemporal(e),
		checkBoltVersionDurable(e),
	)
}

// checkBoltVersionNegotiation adjudicates the raw preamble table and the arms'
// own handshakes.
func checkBoltVersionNegotiation(e *BoltVersionMatrixEvidence) []Violation {
	var v []Violation

	// The tripwire. Every expectation in the table below follows from this exact
	// support list, so a change to it must be a loud failure here rather than a
	// silent staleness there.
	if !slices.Equal(e.Supported, boltVersionSupportedTripwire) {
		v = append(v, boltVersionViolation(ViolationOracleDeviation, "negotiate-supported-list",
			fmt.Sprintf("proto.SupportedVersions is %v, but this scenario's expectation table was written "+
				"against %v; every negotiate-outcome row below encodes an outcome derived from the second "+
				"list, so review them before updating this tripwire",
				boltVersionRenderVersions(e.Supported), boltVersionRenderVersions(boltVersionSupportedTripwire))))
	}

	if len(e.Negotiations) != len(boltVersionNegotiationCases) {
		v = append(v, boltVersionViolation(ViolationOracleDeviation, "negotiate-roster",
			fmt.Sprintf("ran %d negotiation case(s), want %d", len(e.Negotiations), len(boltVersionNegotiationCases))))
		return v
	}
	for i := range boltVersionNegotiationCases {
		want := &boltVersionNegotiationCases[i]
		got := &e.Negotiations[i]
		if got.Name != want.Name {
			v = append(v, boltVersionViolation(ViolationOracleDeviation, "negotiate-roster",
				fmt.Sprintf("case %d is %q, want %q", i, got.Name, want.Name)))
			continue
		}
		if got.ReadErr != "" {
			v = append(v, boltVersionViolation(ViolationOracleDeviation, "negotiate-reply-arrived",
				fmt.Sprintf("case %q (slots %s): no usable reply: %s", got.Name, got.Slots, got.ReadErr)))
			continue
		}
		if got.Accepted != want.Accept {
			v = append(v, boltVersionViolation(ViolationOracleDeviation, "negotiate-outcome",
				fmt.Sprintf("case %q (slots %s): accepted=%v, want %v (got %s)",
					got.Name, got.Slots, got.Accepted, want.Accept, boltVersionRenderVersion(got.Got))))
			continue
		}
		if want.Accept && got.Got != want.Want {
			v = append(v, boltVersionViolation(ViolationOracleDeviation, "negotiate-outcome",
				fmt.Sprintf("case %q (slots %s): negotiated %s, want %s",
					got.Name, got.Slots, boltVersionRenderVersion(got.Got), boltVersionRenderVersion(want.Want))))
		}
	}
	return append(v, checkBoltVersionArmHandshakes(e)...)
}

// checkBoltVersionArmHandshakes is the anti-trap clause: every arm must prove
// WHICH version it actually negotiated.
//
// It is the failure mode this whole scenario is most exposed to. An arm that
// silently landed on 5.6 when it meant 4.4 would satisfy every other clause
// perfectly — the encodings would be consistent, the semantics invariant, the
// auth contract met — and the 4.4 path would still be untested. So the negotiated
// version is recorded on BOTH connections each arm opens and compared against
// what the arm asked for, and a handshake that was refused outright is a
// violation rather than an arm quietly doing nothing.
func checkBoltVersionArmHandshakes(e *BoltVersionMatrixEvidence) []Violation {
	var v []Violation
	if len(e.Arms) != len(boltVersionTargets) {
		return append(v, boltVersionViolation(ViolationOracleDeviation, "arm-roster",
			fmt.Sprintf("ran %d arm(s), want %d", len(e.Arms), len(boltVersionTargets))))
	}
	for i := range e.Arms {
		a := &e.Arms[i]
		t := &boltVersionTargets[i]
		if a.Name != t.Name || a.Asked != t.Version {
			v = append(v, boltVersionViolation(ViolationOracleDeviation, "arm-roster",
				fmt.Sprintf("arm %d is %q/%s, want %q/%s",
					i, a.Name, boltVersionRenderVersion(a.Asked), t.Name, boltVersionRenderVersion(t.Version))))
			continue
		}
		if a.NegotiateErr != "" {
			v = append(v, boltVersionViolation(ViolationOracleDeviation, "arm-negotiated-version",
				fmt.Sprintf("arm %q: handshake refused (%s), so nothing this arm reports is about %s",
					a.Name, a.NegotiateErr, boltVersionRenderVersion(t.Version))))
			continue
		}
		if a.Canonical != t.Version {
			v = append(v, boltVersionViolation(ViolationOracleDeviation, "arm-negotiated-version",
				fmt.Sprintf("arm %q asked for %s with the canonical spelling and got %s; every observation "+
					"this arm makes would be attributed to the wrong version",
					a.Name, boltVersionRenderVersion(t.Version), boltVersionRenderVersion(a.Canonical))))
		}
		if a.Spelled != t.Version {
			v = append(v, boltVersionViolation(ViolationOracleDeviation, "arm-spelling-invariant",
				fmt.Sprintf("arm %q: the seeded spelling %s negotiated %s, but the canonical spelling "+
					"negotiated %s; the version a client reaches must be a function of what it offered, "+
					"not of how it spelled it",
					a.Name, a.Spelling, boltVersionRenderVersion(a.Spelled), boltVersionRenderVersion(a.Canonical))))
		}
	}
	return v
}

// checkBoltVersionLogonContract adjudicates the authentication split. Every
// expectation comes from the arm's LITERAL DefersAuth column, never from
// recomputing the server's own threshold rule.
func checkBoltVersionLogonContract(e *BoltVersionMatrixEvidence) []Violation {
	var v []Violation
	for i := range e.Arms {
		if i >= len(boltVersionTargets) {
			break
		}
		a := &e.Arms[i]
		if a.NegotiateErr != "" {
			continue
		}
		t := &boltVersionTargets[i]
		v = append(v, checkBoltVersionArmAuth(a, t)...)
	}
	return v
}

// checkBoltVersionArmAuth states the four probes for one arm.
//
// It carries one branch per probe per side of the 5.1 split; the paired shape is
// what makes the contract readable, and splitting it would scatter the pairing.
//
// the paired table is the point; see above.
func checkBoltVersionArmAuth(a *BoltVersionArm, t *boltVersionTarget) []Violation {
	var v []Violation
	p := &a.Auth

	// A correct HELLO always succeeds: on the inline side because it
	// authenticated, on the deferred side because it did not try to.
	if p.HelloGoodKind != boltVersionKindSuccess {
		v = append(v, boltVersionViolation(ViolationOracleDeviation, "logon-hello-good-credentials",
			fmt.Sprintf("arm %q: HELLO with the correct password drew %s %s, want SUCCESS",
				a.Name, p.HelloGoodKind, p.HelloGoodCode)))
	}

	// The discriminating clause: is a RUN served before any LOGON?
	if t.DefersAuth {
		if p.RunAfterHelloKind != boltVersionKindFailure ||
			p.RunAfterHelloCode != boltVersionStateGateCode ||
			!strings.Contains(p.RunAfterHelloMessage, boltVersionInStateAuthentication) {
			v = append(v, boltVersionViolation(ViolationOracleDeviation, "logon-run-before-logon-refused",
				fmt.Sprintf("arm %q defers authentication to LOGON, so a RUN sent straight after HELLO must be "+
					"refused %s naming %q; got %s %s %q",
					a.Name, boltVersionStateGateCode, boltVersionInStateAuthentication,
					p.RunAfterHelloKind, p.RunAfterHelloCode, p.RunAfterHelloMessage)))
		}
	} else if p.RunAfterHelloKind != boltVersionKindSuccess {
		v = append(v, boltVersionViolation(ViolationOracleDeviation, "logon-run-after-inline-hello-served",
			fmt.Sprintf("arm %q authenticates INLINE on HELLO and must reach READY without a LOGON, so the "+
				"following RUN must be served; got %s %s %q",
				a.Name, p.RunAfterHelloKind, p.RunAfterHelloCode, p.RunAfterHelloMessage)))
	}

	// A wrong password: refused and fatal on the inline side, unnoticed on the
	// deferred side. This is the pair that cannot pass against a version-blind
	// server, because the same bytes must produce opposite outcomes.
	if t.DefersAuth {
		if p.HelloWrongKind != boltVersionKindSuccess || !p.WrongHelloConnAlive {
			v = append(v, boltVersionViolation(ViolationOracleDeviation, "logon-wrong-password-on-hello-ignored",
				fmt.Sprintf("arm %q does not authenticate on HELLO, so a WRONG password there must be accepted "+
					"and the connection left intact; got %s %s alive=%v",
					a.Name, p.HelloWrongKind, p.HelloWrongCode, p.WrongHelloConnAlive)))
		}
	} else {
		if p.HelloWrongKind != boltVersionKindFailure || p.HelloWrongCode != boltVersionUnauthorizedCode {
			v = append(v, boltVersionViolation(ViolationOracleDeviation, "logon-wrong-password-on-hello-refused",
				fmt.Sprintf("arm %q authenticates on HELLO, so a WRONG password there must draw %s; got %s %s",
					a.Name, boltVersionUnauthorizedCode, p.HelloWrongKind, p.HelloWrongCode)))
		}
		if p.WrongHelloConnAlive {
			v = append(v, boltVersionViolation(ViolationOracleDeviation, "logon-failed-hello-is-fatal",
				fmt.Sprintf("arm %q: the connection survived a refused HELLO. A failed inline authentication "+
					"makes the session DEFUNCT precisely so the socket cannot be reused for a second attempt "+
					"(task #1345)", a.Name)))
		}
	}

	// A credential-less HELLO, and the LOGON a driver follows it with.
	if t.DefersAuth {
		if p.HelloBareKind != boltVersionKindSuccess {
			v = append(v, boltVersionViolation(ViolationOracleDeviation, "logon-bare-hello-accepted",
				fmt.Sprintf("arm %q: a credential-less HELLO — what a %s driver actually sends — drew %s %s, "+
					"want SUCCESS", a.Name, a.Name, p.HelloBareKind, p.HelloBareCode)))
		}
		if p.LogonKind != boltVersionKindSuccess || p.RunAfterLogonKind != boltVersionKindSuccess {
			v = append(v, boltVersionViolation(ViolationOracleDeviation, "logon-reaches-ready",
				fmt.Sprintf("arm %q: LOGON drew %s and the following RUN drew %s; a correct LOGON must reach "+
					"READY and serve the next statement",
					a.Name, p.LogonKind, p.RunAfterLogonKind)))
		}
	} else if p.HelloBareKind != boltVersionKindFailure || p.HelloBareCode != boltVersionUnauthorizedCode {
		v = append(v, boltVersionViolation(ViolationOracleDeviation, "logon-bare-hello-refused",
			fmt.Sprintf("arm %q carries its credentials ON HELLO, so a credential-less one must draw %s; got %s %s",
				a.Name, boltVersionUnauthorizedCode, p.HelloBareKind, p.HelloBareCode)))
	}

	return append(v, checkBoltVersionResetGate(a, t)...)
}

// checkBoltVersionResetGate states the pre-authentication RESET contract.
func checkBoltVersionResetGate(a *BoltVersionArm, t *boltVersionTarget) []Violation {
	var v []Violation
	p := &a.Auth
	if p.ResetAfterHelloKind != boltVersionKindSuccess {
		return append(v, boltVersionViolation(ViolationOracleDeviation, "logon-reset-accepted",
			fmt.Sprintf("arm %q: RESET after HELLO drew %s, want SUCCESS (RESET is legal from every "+
				"non-DEFUNCT state)", a.Name, p.ResetAfterHelloKind)))
	}
	if t.DefersAuth {
		if p.RunAfterResetKind != boltVersionKindFailure ||
			p.RunAfterResetCode != boltVersionStateGateCode ||
			!strings.Contains(p.RunAfterResetMessage, boltVersionInStateNegotiation) {
			v = append(v, boltVersionViolation(ViolationOracleDeviation, "logon-reset-before-logon-drops-to-negotiation",
				fmt.Sprintf("arm %q: the session had NOT authenticated when it was RESET, so it must return to "+
					"NEGOTIATION and the following RUN must be refused %s naming %q; got %s %s %q. RESET must "+
					"never grant access authentication has not (task #1345)",
					a.Name, boltVersionStateGateCode, boltVersionInStateNegotiation,
					p.RunAfterResetKind, p.RunAfterResetCode, p.RunAfterResetMessage)))
		}
		return v
	}
	if p.RunAfterResetKind != boltVersionKindSuccess {
		v = append(v, boltVersionViolation(ViolationOracleDeviation, "logon-reset-keeps-an-authenticated-session",
			fmt.Sprintf("arm %q: the session HAD authenticated on HELLO, so RESET must clear it to READY and the "+
				"following RUN must be served; got %s %s %q",
				a.Name, p.RunAfterResetKind, p.RunAfterResetCode, p.RunAfterResetMessage)))
	}
	return v
}

// The literal codes and message fragments the refusal clauses pin.
//
// The `in state X` fragment is matched as the WHOLE phrase and not as a bare
// state name, following rmp #2484: `TX_STREAMING` contains `STREAMING`, so a
// containment check on a name alone would accept a refusal from a different
// state. `NEGOTIATION` and `AUTHENTICATION` do not overlap that way, but the
// discipline is kept because the next state added might.
const (
	boltVersionStateGateCode         = "Neo.ClientError.Request.Invalid"
	boltVersionUnauthorizedCode      = "Neo.ClientError.Security.Unauthorized"
	boltVersionInStateAuthentication = "in state AUTHENTICATION"
	boltVersionInStateNegotiation    = "in state NEGOTIATION"
)

// -----------------------------------------------------------------------------
// The encodings
// -----------------------------------------------------------------------------

// boltVersionExpectedWidths returns the (tag, arity) pairs the entity record must
// carry at a given layout, in encounter order: the node, the relationship, then
// the path with its two nodes and one unbound relationship.
//
// The arities are LITERAL, transcribed from the layout table in
// bolt/server/entity_struct.go:21-27, which is itself transcribed from the
// neo4j-go-driver hydrator that asserts them. They are what a real driver
// enforces, so an off-by-one here is a protocol error rather than a cosmetic
// difference.
func boltVersionExpectedWidths(major5 bool) []boltWireWidth {
	node, rel, unbound := 3, 5, 3
	if major5 {
		node, rel, unbound = 4, 8, 4
	}
	return []boltWireWidth{
		{Tag: boltTagNode, Arity: node},
		{Tag: boltTagRelationship, Arity: rel},
		// The path is THREE fields at every version — its node list, its unbound
		// relationship list and its index list — which is the control: the version
		// knob reaches the path only through the members it contains.
		{Tag: boltTagPath, Arity: 3},
		{Tag: boltTagNode, Arity: node},
		{Tag: boltTagNode, Arity: node},
		{Tag: boltTagUnboundRelationship, Arity: unbound},
	}
}

// checkBoltVersionEncodings adjudicates the struct layouts, the element_ids, and
// the guard clause that the two majors do not encode identically.
func checkBoltVersionEncodings(e *BoltVersionMatrixEvidence) []Violation {
	var v []Violation
	for i := range e.Arms {
		if i >= len(boltVersionTargets) {
			break
		}
		a := &e.Arms[i]
		if a.NegotiateErr != "" {
			continue
		}
		t := &boltVersionTargets[i]
		ent := &a.Entity
		if ent.Err != "" {
			v = append(v, boltVersionViolation(ViolationOracleDeviation, "encoding-record-captured",
				fmt.Sprintf("arm %q: %s", a.Name, ent.Err)))
			continue
		}
		want := boltVersionExpectedWidths(t.Major5Layout)
		if !slices.Equal(ent.Census, want) {
			v = append(v, boltVersionViolation(ViolationOracleDeviation, "encoding-struct-layout",
				fmt.Sprintf("arm %q emitted the struct census [%s], want [%s]. Field counts are asserted by a "+
					"real driver's hydrator, so a wrong arity is a protocol error, not a cosmetic difference",
					a.Name, boltWireRenderCensus(ent.Census), boltWireRenderCensus(want))))
		}
		if !slices.Equal(ent.Census, ent.CodecCensus) {
			v = append(v, boltVersionViolation(ViolationOracleDeviation, "encoding-walker-agrees-with-codec",
				fmt.Sprintf("arm %q: the independent PackStream reader saw [%s] and the module's own decoder "+
					"saw [%s] in the SAME bytes; one of the two is wrong and no layout verdict from this run "+
					"can be trusted until it is known which",
					a.Name, boltWireRenderCensus(ent.Census), boltWireRenderCensus(ent.CodecCensus))))
		}
		v = append(v, checkBoltVersionElementIDs(a, t)...)
	}
	return append(v, checkBoltVersionEncodingsDiffer(e)...)
}

// checkBoltVersionElementIDs states the element_id contract on both sides.
//
// At the Bolt 5 layout every entity carries its element_id, and the value is
// DERIVED rather than pinned to a literal: it must be the decimal rendering of
// the id the same structure reports, which is what makes an
// elementId()-reading client and a Node.ElementId-reading client agree. At the
// Bolt 4 layout there must be none at all — the assertion is the ABSENCE, which
// is the half a "the values agree" oracle cannot see.
func checkBoltVersionElementIDs(a *BoltVersionArm, t *boltVersionTarget) []Violation {
	var v []Violation
	ent := &a.Entity
	if !t.Major5Layout {
		if len(ent.ElementIDs) != 0 {
			v = append(v, boltVersionViolation(ViolationOracleDeviation, "encoding-no-element-id-below-bolt-5",
				fmt.Sprintf("arm %q uses the pre-5 entity layout, which has no element_id field, yet %d "+
					"trailing string(s) arrived: %v", a.Name, len(ent.ElementIDs), ent.ElementIDs)))
		}
		return v
	}
	// Encounter order: the node's, the relationship's three (its own, its start's
	// and its end's), then the path's two nodes and its unbound relationship.
	want := []string{
		strconv.FormatInt(ent.NodeID, 10),
		strconv.FormatInt(ent.RelID, 10),
		strconv.FormatInt(ent.RelStart, 10),
		strconv.FormatInt(ent.RelEnd, 10),
	}
	for _, id := range ent.PathNodeIDs {
		want = append(want, strconv.FormatInt(id, 10))
	}
	for _, id := range ent.PathRelIDs {
		want = append(want, strconv.FormatInt(id, 10))
	}
	if !slices.Equal(ent.ElementIDs, want) {
		v = append(v, boltVersionViolation(ViolationOracleDeviation, "encoding-element-id-is-the-decimal-id",
			fmt.Sprintf("arm %q carried element_ids %v, want %v — each must be the decimal rendering of the id "+
				"the same structure reports, so that elementId() in a projection and Node.ElementId off the "+
				"wire name the same entity", a.Name, ent.ElementIDs, want)))
	}
	return v
}

// checkBoltVersionEncodingsDiffer is the GUARD on every invariance clause.
//
// Each of those clauses requires two versions to agree, and a server that ignored
// the negotiated version entirely — emitting Bolt 5 structures to a 4.4 client —
// would satisfy all of them perfectly while breaking every real 4.4 driver, whose
// hydrator asserts the field count. So the two majors are required to DIFFER, on
// two independent witnesses: the struct census and the raw byte length.
//
// Both are stated because either alone is weak. Two records can differ in length
// without differing in layout, and a census could in principle coincide. Together
// they are a positive statement that the version reached the encoder.
func checkBoltVersionEncodingsDiffer(e *BoltVersionMatrixEvidence) []Violation {
	var v []Violation
	var four, five *BoltVersionArm
	for i := range e.Arms {
		a := &e.Arms[i]
		if a.NegotiateErr != "" || a.Entity.Err != "" {
			continue
		}
		if a.Asked.Major == 4 && four == nil {
			four = a
		}
		if a.Asked.Major == 5 && five == nil {
			five = a
		}
	}
	if four == nil || five == nil {
		// Not a contract failure: the non-vacuity gate reports an unconstructed
		// comparison, which is what this is.
		return v
	}
	if slices.Equal(four.Entity.Census, five.Entity.Census) {
		v = append(v, boltVersionViolation(ViolationOracleDeviation, "encoding-differs-across-majors",
			fmt.Sprintf("arms %q and %q produced the IDENTICAL struct census [%s]. The encoder branches on the "+
				"major version, so an identical census means the negotiated version did not reach it — and "+
				"every 'the decoded values agree' clause in this scenario would pass against exactly that "+
				"server while a real 4.4 driver failed to hydrate the reply",
				four.Name, five.Name, boltWireRenderCensus(four.Entity.Census))))
	}
	if four.Entity.Bytes == five.Entity.Bytes {
		v = append(v, boltVersionViolation(ViolationOracleDeviation, "encoding-differs-across-majors",
			fmt.Sprintf("arms %q and %q produced records of the SAME length (%d bytes); the Bolt 5 layout adds "+
				"five element_id strings to this record and cannot encode to the same size",
				four.Name, five.Name, four.Entity.Bytes)))
	}
	return v
}

// -----------------------------------------------------------------------------
// Version-invariant semantics
// -----------------------------------------------------------------------------

// checkBoltVersionInvariance requires every arm to have observed the SAME
// semantics as the first arm: the same entity core, the same round-tripped
// parameter values and types, the same refusals, and a committed transaction.
//
// The first arm is the reference for no reason other than determinism of the
// message; the relation asserted is equality across the whole set, so which
// member is named as the reference does not change what passes.
func checkBoltVersionInvariance(e *BoltVersionMatrixEvidence) []Violation {
	var v []Violation
	ref := boltVersionReferenceArm(e)
	if ref == nil {
		return v
	}
	for i := range e.Arms {
		a := &e.Arms[i]
		if a.NegotiateErr != "" {
			continue
		}
		v = append(v, checkBoltVersionArmInvariance(a, ref)...)
		v = append(v, checkBoltVersionArmTransaction(a, i)...)
		v = append(v, checkBoltVersionArmAbuse(a)...)
	}
	return v
}

// boltVersionReferenceArm returns the first arm whose captures completed, or nil.
func boltVersionReferenceArm(e *BoltVersionMatrixEvidence) *BoltVersionArm {
	for i := range e.Arms {
		a := &e.Arms[i]
		if a.NegotiateErr == "" && a.Entity.Err == "" && a.ParamErr == "" {
			return a
		}
	}
	return nil
}

// checkBoltVersionArmInvariance compares one arm's semantics against the
// reference arm's.
func checkBoltVersionArmInvariance(a, ref *BoltVersionArm) []Violation {
	var v []Violation
	if a.Entity.Err == "" && ref.Entity.Err == "" {
		if diff := boltVersionEntityCoreDiff(&a.Entity, &ref.Entity); diff != "" {
			v = append(v, boltVersionViolation(ViolationOracleDeviation, "invariant-entity-core",
				fmt.Sprintf("arm %q read a DIFFERENT graph from arm %q over the same fixture: %s. The wire "+
					"encoding is version-dependent; what it encodes is not",
					a.Name, ref.Name, diff)))
		}
	}
	if a.ParamErr != "" {
		v = append(v, boltVersionViolation(ViolationOracleDeviation, "invariant-parameter-roundtrip",
			fmt.Sprintf("arm %q: parameter round trip failed: %s", a.Name, a.ParamErr)))
		return v
	}
	if ref.ParamErr != "" || len(a.Params) != len(ref.Params) {
		return v
	}
	for k := range a.Params {
		if reflect.DeepEqual(a.Params[k], ref.Params[k]) {
			continue
		}
		v = append(v, boltVersionViolation(ViolationOracleDeviation, "invariant-parameter-roundtrip",
			fmt.Sprintf("arm %q round-tripped the %q parameter as %T %#v, but arm %q got %T %#v. The concrete "+
				"Go type is compared as well as the value, because the dynamic type IS the wire encoding: an "+
				"Integer re-encoded as the identically-rendered Float would otherwise pass",
				a.Name, boltVersionParamCases[k].Name, a.Params[k], a.Params[k],
				ref.Name, ref.Params[k], ref.Params[k])))
	}
	return v
}

// boltVersionEntityCoreDiff reports the first way two arms' entity cores differ,
// or the empty string when they agree.
func boltVersionEntityCoreDiff(got, want *BoltVersionEntity) string {
	switch {
	case got.NodeID != want.NodeID:
		return fmt.Sprintf("node id %d vs %d", got.NodeID, want.NodeID)
	case !slices.Equal(got.NodeLabels, want.NodeLabels):
		return fmt.Sprintf("node labels %v vs %v", got.NodeLabels, want.NodeLabels)
	case !reflect.DeepEqual(got.NodeProps, want.NodeProps):
		return fmt.Sprintf("node properties %#v vs %#v", got.NodeProps, want.NodeProps)
	case got.RelID != want.RelID || got.RelStart != want.RelStart || got.RelEnd != want.RelEnd:
		return fmt.Sprintf("relationship %d(%d->%d) vs %d(%d->%d)",
			got.RelID, got.RelStart, got.RelEnd, want.RelID, want.RelStart, want.RelEnd)
	case got.RelType != want.RelType:
		return fmt.Sprintf("relationship type %q vs %q", got.RelType, want.RelType)
	case !reflect.DeepEqual(got.RelProps, want.RelProps):
		return fmt.Sprintf("relationship properties %#v vs %#v", got.RelProps, want.RelProps)
	case !slices.Equal(got.PathNodeIDs, want.PathNodeIDs):
		return fmt.Sprintf("path node ids %v vs %v", got.PathNodeIDs, want.PathNodeIDs)
	case !slices.Equal(got.PathRelIDs, want.PathRelIDs):
		return fmt.Sprintf("path relationship ids %v vs %v", got.PathRelIDs, want.PathRelIDs)
	case !slices.Equal(got.PathIndices, want.PathIndices):
		return fmt.Sprintf("path indices %v vs %v", got.PathIndices, want.PathIndices)
	}
	return ""
}

// checkBoltVersionArmTransaction states that an explicit transaction commits at
// every version and that the marker census advances by exactly one per arm.
//
// The count is a DERIVED expectation — the arm's ordinal plus one — rather than a
// literal, so it states that this arm's write landed AND that no earlier arm's
// write was lost or duplicated by the version it was written over.
func checkBoltVersionArmTransaction(a *BoltVersionArm, ordinal int) []Violation {
	var v []Violation
	if a.TxErr != "" {
		return append(v, boltVersionViolation(ViolationOracleDeviation, "invariant-explicit-transaction",
			fmt.Sprintf("arm %q: %s", a.Name, a.TxErr)))
	}
	if !a.TxCommitted {
		return append(v, boltVersionViolation(ViolationACIDAtomicity, "invariant-explicit-transaction",
			fmt.Sprintf("arm %q: the explicit transaction did not commit", a.Name)))
	}
	if !boltVersionBookmarkShapeOK(a.TxBookmark) {
		v = append(v, boltVersionViolation(ViolationOracleDeviation, "invariant-commit-bookmark-shape",
			fmt.Sprintf("arm %q: COMMIT returned bookmark %q, want %q followed by 8 hex digits",
				a.Name, a.TxBookmark, bookmarkPrefix)))
	}
	if want := ordinal + 1; a.TxCountAfter != want {
		v = append(v, boltVersionViolation(ViolationACIDDurability, "invariant-marker-census-advances",
			fmt.Sprintf("arm %q saw %d marker node(s) after committing its own, want %d (one per arm so far). "+
				"A short count means an earlier version's write was lost; a long one means it was duplicated",
				a.Name, a.TxCountAfter, want)))
	}
	return v
}

// boltVersionBookmarkShapeOK reports whether bm has the shape
// [server.NextBookmark] produces: the prefix followed by exactly 8 hex digits.
//
// Only the SHAPE is asserted. The literal text comes from a PROCESS-GLOBAL
// counter (bolt/server/bookmark.go:13), so it depends on how many transactions
// every other test in the process committed first and is not reachable from the
// seed.
func boltVersionBookmarkShapeOK(bm string) bool {
	if !strings.HasPrefix(bm, bookmarkPrefix) {
		return false
	}
	digits := strings.TrimPrefix(bm, bookmarkPrefix)
	if len(digits) != 8 {
		return false
	}
	_, err := strconv.ParseUint(digits, 16, 64)
	return err == nil
}

// checkBoltVersionArmAbuse states that the bad-actor battery draws the SAME typed
// refusal at every version, pinned to the literal code and message.
//
// The literal matters: two versions agreeing on a WRONG answer would satisfy a
// pure cross-version comparison. None of these three paths reads the negotiated
// version, so a version-dependent refusal here would be a genuine surprise.
func checkBoltVersionArmAbuse(a *BoltVersionArm) []Violation {
	var v []Violation
	if len(a.Abuse) != len(boltVersionAbuseCases) {
		return append(v, boltVersionViolation(ViolationOracleDeviation, "invariant-abuse-roster",
			fmt.Sprintf("arm %q ran %d abuse probe(s), want %d", a.Name, len(a.Abuse), len(boltVersionAbuseCases))))
	}
	for i, want := range boltVersionAbuseCases {
		got := &a.Abuse[i]
		if got.Kind != boltVersionKindFailure || got.Code != want.Code || got.Message != want.Message {
			v = append(v, boltVersionViolation(ViolationOracleDeviation, "invariant-abuse-refusal",
				fmt.Sprintf("arm %q: probe %q drew %s %s %q, want FAILURE %s %q",
					a.Name, want.Name, got.Kind, got.Code, got.Message, want.Code, want.Message)))
		}
	}
	return v
}

// -----------------------------------------------------------------------------
// Temporal encodings
// -----------------------------------------------------------------------------

// boltVersionExpectTemporal returns the structure tag, the integer fields and the
// zone string one temporal kind must carry at a given layout, computed from the
// HARNESS's own reference rather than from the encoder.
//
// The Bolt 4 legacy convention is stated as `utc + offset`, which is the wall
// clock read as if it were UTC — the same relation
// bolt/server/session.go:2238-2243 implements, but derived here from Go's own
// time package rather than copied from that code.
func boltVersionExpectTemporal(kind string, major5 bool, ref *BoltVersionTemporalRefs) (tag byte, ints []int64, zone string) {
	switch kind {
	case "date":
		return 0x44, []int64{ref.EpochDay}, ""
	case "localtime":
		return 0x74, []int64{ref.NanosOfDay}, ""
	case "time":
		return 0x54, []int64{ref.NanosOfDay, ref.OffsetSeconds}, ""
	case "localdatetime":
		return 0x64, []int64{ref.LocalAsUTCSec, ref.OffsetNanos}, ""
	case "datetime-offset":
		if major5 {
			return 0x49, []int64{ref.OffsetUTCSec, ref.OffsetNanos, ref.OffsetSeconds}, ""
		}
		return 0x46, []int64{ref.OffsetUTCSec + ref.OffsetSeconds, ref.OffsetNanos, ref.OffsetSeconds}, ""
	case "datetime-named":
		if major5 {
			return 0x69, []int64{ref.Named.UTCSeconds, ref.Named.Nanos}, boltVersionZoneName
		}
		return 0x66, []int64{ref.Named.UTCSeconds + ref.Named.OffsetSeconds, ref.Named.Nanos}, boltVersionZoneName
	default:
		return 0x45, ref.Duration, ""
	}
}

// checkBoltVersionTemporal adjudicates every temporal column at every version.
func checkBoltVersionTemporal(e *BoltVersionMatrixEvidence) []Violation {
	var v []Violation
	for i := range e.Arms {
		if i >= len(boltVersionTargets) {
			break
		}
		a := &e.Arms[i]
		if a.NegotiateErr != "" {
			continue
		}
		t := &boltVersionTargets[i]
		if len(a.Temporals) != len(boltVersionTemporalKinds) {
			v = append(v, boltVersionViolation(ViolationOracleDeviation, "temporal-roster",
				fmt.Sprintf("arm %q captured %d temporal(s), want %d",
					a.Name, len(a.Temporals), len(boltVersionTemporalKinds))))
			continue
		}
		for k := range a.Temporals {
			got := &a.Temporals[k]
			if got.Kind == "datetime-named" && !e.NamedZoneResolved {
				// The reference could not be computed; the non-vacuity gate reports
				// the unconstructed clause rather than this one inventing a verdict.
				continue
			}
			if got.Err != "" {
				v = append(v, boltVersionViolation(ViolationOracleDeviation, "temporal-captured",
					fmt.Sprintf("arm %q, %s: %s", a.Name, got.Kind, got.Err)))
				continue
			}
			wantTag, wantInts, wantZone := boltVersionExpectTemporal(got.Kind, t.Major5Layout, &e.TemporalRef)
			clause := "temporal-version-invariant"
			if got.Kind == "datetime-offset" || got.Kind == "datetime-named" {
				clause = "temporal-zoned-datetime-convention"
			}
			if got.Tag != wantTag {
				v = append(v, boltVersionViolation(ViolationOracleDeviation, clause,
					fmt.Sprintf("arm %q, %s: structure tag 0x%02X, want 0x%02X", a.Name, got.Kind, got.Tag, wantTag)))
			}
			if !slices.Equal(got.Ints, wantInts) {
				v = append(v, boltVersionViolation(ViolationOracleDeviation, clause,
					fmt.Sprintf("arm %q, %s: fields %v, want %v (computed by the harness from the same literal "+
						"the query carries)", a.Name, got.Kind, got.Ints, wantInts)))
			}
			if got.Zone != wantZone {
				v = append(v, boltVersionViolation(ViolationOracleDeviation, clause,
					fmt.Sprintf("arm %q, %s: zone %q, want %q", a.Name, got.Kind, got.Zone, wantZone)))
			}
		}
	}
	return v
}

// -----------------------------------------------------------------------------
// Durability
// -----------------------------------------------------------------------------

// checkBoltVersionDurable states that the protocol version a write arrived over
// does not reach the durable state: each arm's marker node must be present in the
// live engine AND in a graph reopened through real WAL recovery, exactly once.
func checkBoltVersionDurable(e *BoltVersionMatrixEvidence) []Violation {
	var v []Violation
	for i := range boltVersionTargets {
		name := boltVersionTargets[i].Name
		live, recovered := e.LiveMarkers[name], e.RecoveredMarkers[name]
		if live != 1 {
			v = append(v, boltVersionViolation(ViolationACIDDurability, "durable-marker-live",
				fmt.Sprintf("the marker committed over Bolt %s is present %d time(s) in the live engine, want 1",
					name, live)))
		}
		if recovered != live {
			v = append(v, boltVersionViolation(ViolationACIDDurability, "durable-survives-recovery",
				fmt.Sprintf("the marker committed over Bolt %s is present %d time(s) live but %d time(s) after "+
					"real WAL recovery; an acknowledged commit must survive the crash whatever protocol version "+
					"carried it", name, live, recovered)))
		}
	}
	return v
}

// -----------------------------------------------------------------------------
// Non-vacuity
// -----------------------------------------------------------------------------

// checkBoltVersionMatrixNonVacuity answers a DIFFERENT question from the contract
// checkers: was this run in a position to notice a defect at all?
//
// Its shortfalls fail the scenario just as a contract violation does, because a
// clause that could not have fired has not passed — it has not run. The whole
// point of a version matrix is that it actually entered each version's path, and
// the failure mode that would quietly destroy it is an arm negotiating 5.6 while
// believing it negotiated 4.4. That is caught twice: as a contract violation by
// `arm-negotiated-version`, and here as a census of which versions and which
// distinguishing observations the run actually obtained.
func checkBoltVersionMatrixNonVacuity(e *BoltVersionMatrixEvidence) []Violation {
	return slices.Concat(
		checkBoltVersionCoverageNonVacuity(e),
		checkBoltVersionDeltaNonVacuity(e),
		checkBoltVersionProbeNonVacuity(e),
	)
}

// checkBoltVersionCoverageNonVacuity requires the matrix to have been entered:
// every target negotiated, both majors reached, and both sides of the
// authentication split exercised with the layout held fixed.
func checkBoltVersionCoverageNonVacuity(e *BoltVersionMatrixEvidence) []Violation {
	var v []Violation
	negotiated := map[proto.Version]bool{}
	for i := range e.Arms {
		a := &e.Arms[i]
		if a.NegotiateErr == "" && a.Canonical == a.Asked && a.Spelled == a.Asked {
			negotiated[a.Asked] = true
		}
	}
	var missing []string
	for i := range boltVersionTargets {
		if !negotiated[boltVersionTargets[i].Version] {
			missing = append(missing, boltVersionTargets[i].Name)
		}
	}
	if len(missing) > 0 {
		v = append(v, boltVersionViolation(ViolationVacuousRun, "nv-every-target-negotiated",
			fmt.Sprintf("the run did not confirm negotiation of %v; a version-matrix arm that did not reach "+
				"its version has tested the version it DID reach, twice", missing)))
	}

	// The crossed design, asserted rather than assumed: an inline-auth arm on the
	// Bolt 5 layout (5.0) is what separates the auth axis from the encoding axis.
	// Without it, every difference this scenario measures is attributable to
	// either axis and to neither in particular.
	var sawInlineOnMajor5, sawDeferred, sawMajor4 bool
	for i := range boltVersionTargets {
		t := &boltVersionTargets[i]
		if !negotiated[t.Version] {
			continue
		}
		switch {
		case !t.Major5Layout:
			sawMajor4 = true
		case !t.DefersAuth:
			sawInlineOnMajor5 = true
		default:
			sawDeferred = true
		}
	}
	if !sawMajor4 || !sawInlineOnMajor5 || !sawDeferred {
		v = append(v, boltVersionViolation(ViolationVacuousRun, "nv-crossed-design-constructed",
			fmt.Sprintf("the run reached major-4 layout=%v, inline auth on the major-5 layout=%v, deferred "+
				"auth=%v; all three are needed for a difference to be attributable to ONE axis rather than to "+
				"the pair", sawMajor4, sawInlineOnMajor5, sawDeferred)))
	}
	return v
}

// checkBoltVersionDeltaNonVacuity requires the run to have OBSERVED the
// differences the invariance clauses are guarded by, and the negotiation table to
// have exercised its interesting rows.
func checkBoltVersionDeltaNonVacuity(e *BoltVersionMatrixEvidence) []Violation {
	var v []Violation

	arities := map[boltWireWidth]bool{}
	tags := map[byte]bool{}
	var captured int
	for i := range e.Arms {
		a := &e.Arms[i]
		if a.NegotiateErr != "" || a.Entity.Err != "" {
			continue
		}
		captured++
		for _, w := range a.Entity.Census {
			arities[w] = true
		}
		for k := range a.Temporals {
			if a.Temporals[k].Err == "" {
				tags[a.Temporals[k].Tag] = true
			}
		}
	}
	if captured < 2 {
		v = append(v, boltVersionViolation(ViolationVacuousRun, "nv-entity-records-captured",
			fmt.Sprintf("only %d arm(s) captured an entity record; the encoding-difference guard needs at "+
				"least two to compare", captured)))
	}
	// The same tag at two different arities is the direct evidence that the
	// layout branch was taken; a run that saw only one arity per tag cannot have
	// distinguished the two majors, whatever the arms believed they negotiated.
	if !boltVersionSawTwoArities(arities, boltTagNode) || !boltVersionSawTwoArities(arities, boltTagRelationship) {
		v = append(v, boltVersionViolation(ViolationVacuousRun, "nv-layout-branch-observed",
			fmt.Sprintf("the run never saw the same entity tag at two different arities (census union: [%s]); "+
				"the pre-5 and 5+ layouts were therefore not both exercised",
				boltWireRenderCensus(boltVersionSortedWidths(arities)))))
	}
	// The same for the temporal branch: both spellings of a zoned datetime.
	if !tags[0x49] || !tags[0x46] || !tags[0x69] || !tags[0x66] {
		v = append(v, boltVersionViolation(ViolationVacuousRun, "nv-temporal-branch-observed",
			fmt.Sprintf("the run did not see both conventions of a zoned datetime (offset 0x49=%v/0x46=%v, "+
				"named 0x69=%v/0x66=%v); the UTC-versus-legacy branch was not both-ways exercised",
				tags[0x49], tags[0x46], tags[0x69], tags[0x66])))
	}
	if !e.NamedZoneResolved {
		v = append(v, boltVersionViolation(ViolationVacuousRun, "nv-named-zone-reference-available",
			fmt.Sprintf("the zone database could not resolve %q, so the harness has no independent reference "+
				"for the named-zone datetime and its clause was skipped rather than passed",
				boltVersionZoneName)))
	}

	var accepted, refused, ranged int
	for i := range e.Negotiations {
		if e.Negotiations[i].ReadErr != "" {
			continue
		}
		if e.Negotiations[i].Accepted {
			accepted++
		} else {
			refused++
		}
		if strings.Contains(e.Negotiations[i].Slots, "~") {
			ranged++
		}
	}
	if accepted == 0 || refused == 0 || ranged == 0 {
		v = append(v, boltVersionViolation(ViolationVacuousRun, "nv-negotiation-table-exercised",
			fmt.Sprintf("the negotiation table produced %d acceptance(s), %d refusal(s) and %d range offer(s); "+
				"a table that never saw a refusal cannot tell 'the server accepted the right version' from "+
				"'the server accepts everything'", accepted, refused, ranged)))
	}
	return v
}

// boltVersionSawTwoArities reports whether the census union holds the given tag
// at two or more distinct arities.
func boltVersionSawTwoArities(seen map[boltWireWidth]bool, tag byte) bool {
	n := 0
	for w := range seen {
		if w.Tag == tag {
			n++
		}
	}
	return n >= 2
}

// boltVersionSortedWidths renders a census union in a stable order.
func boltVersionSortedWidths(seen map[boltWireWidth]bool) []boltWireWidth {
	out := make([]boltWireWidth, 0, len(seen))
	for w := range seen {
		out = append(out, w)
	}
	slices.SortFunc(out, func(x, y boltWireWidth) int {
		if x.Tag != y.Tag {
			return int(x.Tag) - int(y.Tag)
		}
		return x.Arity - y.Arity
	})
	return out
}

// checkBoltVersionProbeNonVacuity requires each per-arm probe family to have
// produced something to adjudicate.
func checkBoltVersionProbeNonVacuity(e *BoltVersionMatrixEvidence) []Violation {
	var v []Violation
	var spellingsDiffered, committed int
	for i := range e.Arms {
		a := &e.Arms[i]
		if a.NegotiateErr != "" {
			continue
		}
		if a.SpellingDiffers {
			spellingsDiffered++
		}
		if a.TxCommitted {
			committed++
		}
		if len(a.Params) != len(boltVersionParamCases) || a.ParamErr != "" {
			v = append(v, boltVersionViolation(ViolationVacuousRun, "nv-parameter-matrix-round-tripped",
				fmt.Sprintf("arm %q round-tripped %d of %d parameter kinds (%s); the invariance clause compares "+
					"only what was captured", a.Name, len(a.Params), len(boltVersionParamCases), a.ParamErr)))
		}
	}
	if spellingsDiffered == 0 {
		v = append(v, boltVersionViolation(ViolationVacuousRun, "nv-seeded-spelling-differed",
			fmt.Sprintf("seed %#x drew the CANONICAL spelling for every arm, so `arm-spelling-invariant` "+
				"compared each handshake with itself and the range and slot-placement paths were never "+
				"entered", e.Seed)))
	}
	if committed != len(boltVersionTargets) {
		v = append(v, boltVersionViolation(ViolationVacuousRun, "nv-every-version-committed",
			fmt.Sprintf("%d of %d arms committed an explicit transaction; the durable census can only attribute "+
				"a marker to a version that wrote one", committed, len(boltVersionTargets))))
	}
	return v
}

// -----------------------------------------------------------------------------
// Rendering
// -----------------------------------------------------------------------------

// boltVersionRenderVersion renders a version, with a distinct spelling for the
// zero value so "refused" cannot read as "negotiated 0.0".
func boltVersionRenderVersion(ver proto.Version) string {
	if ver == (proto.Version{}) {
		return "<none>"
	}
	return fmt.Sprintf("%d.%d", ver.Major, ver.Minor)
}

// boltVersionRenderVersions renders a version list.
func boltVersionRenderVersions(vs []proto.Version) string {
	parts := make([]string, len(vs))
	for i, ver := range vs {
		parts[i] = boltVersionRenderVersion(ver)
	}
	return strings.Join(parts, " ")
}

// String renders the evidence as a stable, human-readable block.
//
// # What is deliberately EXCLUDED, and why
//
// The rendering is compared byte for byte by
// [TestBoltVersionMatrix_Deterministic], so anything not reachable from the seed
// must stay out of it or that test measures the machine rather than the server:
//
//   - the COMMIT bookmark's literal text, whose counter is process-global
//     (bolt/server/bookmark.go:13) and therefore depends on how many transactions
//     every other test in the process committed first. It is rendered as a
//     positional token and asserted only on its shape, exactly as rmp #2485 does;
//   - connection ids, which are per-connection random;
//   - wall-clock durations, of which this scenario records none at all: no clause
//     here is a deadline, so there is nothing to render;
//   - entity IDS and the record's byte LENGTH. This one was MEASURED, not
//     assumed, and an earlier draft of this godoc claimed the opposite. Two runs
//     of the same seed produced node ids 38/215 and then 227/48, and records of
//     138 and then 140 bytes: the id is derived from a node key minted from a
//     PROCESS-GLOBAL counter, so it depends on how many nodes every other test in
//     the process created first, and the byte length follows it through the
//     decimal element_id strings. Both are rendered POSITIONALLY instead — `n0`,
//     `n1`, `e0` in first-encounter order — which keeps the structural
//     information (which entity appears where, and which element_id belongs to
//     which id) while dropping the process-dependent value.
//
// The CHECKERS still read the raw ids and the raw byte length, and are entitled
// to: every clause over them is a DERIVED relation — the element_id equals the
// decimal of the id in the same structure, the two majors differ in length — and
// never a literal.
func (e *BoltVersionMatrixEvidence) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "bolt-version-matrix seed=%#x\n", e.Seed)
	fmt.Fprintf(&b, "  supported: %s\n", boltVersionRenderVersions(e.Supported))
	fmt.Fprintf(&b, "  named-zone reference resolved: %v\n", e.NamedZoneResolved)

	b.WriteString("  negotiation table:\n")
	for i := range e.Negotiations {
		n := &e.Negotiations[i]
		outcome := "REFUSED"
		if n.Accepted {
			outcome = boltVersionRenderVersion(n.Got)
		}
		if n.ReadErr != "" {
			outcome = "ERR:" + n.ReadErr
		}
		fmt.Fprintf(&b, "    %-34s offers=%-16s -> %s\n", n.Name, n.Slots, outcome)
	}

	for i := range e.Arms {
		e.renderArm(&b, &e.Arms[i])
	}

	b.WriteString("  markers (live/recovered):\n")
	for i := range boltVersionTargets {
		name := boltVersionTargets[i].Name
		fmt.Fprintf(&b, "    %-6s %d/%d\n", name, e.LiveMarkers[name], e.RecoveredMarkers[name])
	}
	return b.String()
}

// renderArm renders one arm.
func (e *BoltVersionMatrixEvidence) renderArm(b *strings.Builder, a *BoltVersionArm) {
	fmt.Fprintf(b, "  arm %s: asked=%s canonical=%s spelled=%s via %s%s\n",
		a.Name, boltVersionRenderVersion(a.Asked), boltVersionRenderVersion(a.Canonical),
		boltVersionRenderVersion(a.Spelled), a.Spelling, boltVersionRenderErr(a.NegotiateErr))
	p := &a.Auth
	fmt.Fprintf(b, "    auth: hello(good)=%s run-after-hello=%s%s hello(wrong)=%s%s alive=%v\n",
		p.HelloGoodKind, p.RunAfterHelloKind, boltVersionRenderCode(p.RunAfterHelloCode),
		p.HelloWrongKind, boltVersionRenderCode(p.HelloWrongCode), p.WrongHelloConnAlive)
	fmt.Fprintf(b, "    auth: hello(bare)=%s%s logon=%s run-after-logon=%s reset=%s run-after-reset=%s%s\n",
		p.HelloBareKind, boltVersionRenderCode(p.HelloBareCode), p.LogonKind, p.RunAfterLogonKind,
		p.ResetAfterHelloKind, p.RunAfterResetKind, boltVersionRenderCode(p.RunAfterResetCode))
	ent := &a.Entity
	if ent.Err != "" {
		fmt.Fprintf(b, "    entity: ERR %s\n", ent.Err)
	} else {
		fmt.Fprintf(b, "    entity: census=[%s]\n", boltWireRenderCensus(ent.Census))
		fmt.Fprintf(b, "    entity: %s\n", boltVersionRenderEntity(ent))
	}
	for k := range a.Temporals {
		t := &a.Temporals[k]
		fmt.Fprintf(b, "    temporal %-16s tag=0x%02X ints=%v zone=%q%s\n",
			t.Kind, t.Tag, t.Ints, t.Zone, boltVersionRenderErr(t.Err))
	}
	fmt.Fprintf(b, "    params: %s%s\n", boltVersionRenderParams(a.Params), boltVersionRenderErr(a.ParamErr))
	fmt.Fprintf(b, "    tx: committed=%v bookmark=%s markers=%d%s\n",
		a.TxCommitted, boltVersionRenderBookmark(a.TxBookmark), a.TxCountAfter, boltVersionRenderErr(a.TxErr))
	for k := range a.Abuse {
		ab := &a.Abuse[k]
		fmt.Fprintf(b, "    abuse %-28s %s %s %q\n", ab.Name, ab.Kind, ab.Code, ab.Message)
	}
}

// boltVersionRenderEntity renders the entity capture with every id replaced by a
// POSITIONAL token.
//
// Node ids are `n0`, `n1`, … and relationship ids `e0`, `e1`, … in
// first-encounter order, and an element_id renders as the token of the id whose
// decimal it is. The structural information is fully preserved — an element_id
// pointing at the wrong entity, or a path visiting the endpoints in the wrong
// order, still shows — while the process-dependent numeric value does not reach
// a rendering the determinism clause compares byte for byte. See
// [BoltVersionMatrixEvidence.String] for the measurement behind that.
func boltVersionRenderEntity(ent *BoltVersionEntity) string {
	nodes := map[int64]string{}
	rels := map[int64]string{}
	token := func(m map[int64]string, prefix string, id int64) string {
		if t, ok := m[id]; ok {
			return t
		}
		t := fmt.Sprintf("%s%d", prefix, len(m))
		m[id] = t
		return t
	}
	n := func(id int64) string { return token(nodes, "n", id) }
	e := func(id int64) string { return token(rels, "e", id) }

	// Assign tokens in the order the record itself presents the entities, so the
	// numbering is a property of the reply rather than of this function.
	node := n(ent.NodeID)
	rel := e(ent.RelID)
	relStart, relEnd := n(ent.RelStart), n(ent.RelEnd)
	pathNodes := make([]string, len(ent.PathNodeIDs))
	for i, id := range ent.PathNodeIDs {
		pathNodes[i] = n(id)
	}
	pathRels := make([]string, len(ent.PathRelIDs))
	for i, id := range ent.PathRelIDs {
		pathRels[i] = e(id)
	}
	// An element_id is the DECIMAL of some id, so it renders as that id's token.
	// One that matches nothing renders verbatim, which is how a wrong element_id
	// stays visible instead of being normalised away.
	byDecimal := map[string]string{}
	for id, t := range nodes {
		byDecimal[strconv.FormatInt(id, 10)] = t
	}
	for id, t := range rels {
		byDecimal[strconv.FormatInt(id, 10)] = t
	}
	elems := make([]string, len(ent.ElementIDs))
	for i, s := range ent.ElementIDs {
		if t, ok := byDecimal[s]; ok {
			elems[i] = t
			continue
		}
		elems[i] = strconv.Quote(s)
	}
	return fmt.Sprintf("node=%s%v rel=%s(%s->%s):%s path=%v/%v/%v elementIds=[%s]",
		node, ent.NodeLabels, rel, relStart, relEnd, ent.RelType,
		pathNodes, pathRels, ent.PathIndices, strings.Join(elems, " "))
}

// boltVersionRenderErr renders an error slot, empty when there was none.
func boltVersionRenderErr(s string) string {
	if s == "" {
		return ""
	}
	return " ERR:" + s
}

// boltVersionRenderCode renders a failure code slot, empty when there was none.
func boltVersionRenderCode(s string) string {
	if s == "" {
		return ""
	}
	return "(" + s + ")"
}

// boltVersionRenderParams renders the round-tripped parameter values with their
// concrete Go types, because the type is part of what the clause compares.
func boltVersionRenderParams(vals []any) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = fmt.Sprintf("%s=%T %#v", boltVersionParamCases[i].Name, v, v)
	}
	return strings.Join(parts, " ")
}

// boltVersionRenderBookmark renders a bookmark POSITIONALLY. Its literal text is
// process-global and therefore not reachable from the seed; see
// [BoltVersionMatrixEvidence.String].
func boltVersionRenderBookmark(bm string) string {
	switch {
	case bm == "":
		return "<empty>"
	case boltVersionBookmarkShapeOK(bm):
		return "<issued>"
	default:
		return "<malformed>"
	}
}

// -----------------------------------------------------------------------------
// The scenario
// -----------------------------------------------------------------------------

// boltVersionMatrixDefaultSeed is the catalogue default. It must differ from
// [boltVersionSeedMix]; see that constant.
const boltVersionMatrixDefaultSeed = 0x2486_B00C

// boltVersionMatrixScenario drives the deterministic protocol-version matrix: the
// raw negotiation table, the authentication split across the Bolt 5.1 boundary,
// the entity and temporal encodings across the Bolt 5 boundary, and the semantics
// that must hold identically at every version.
func boltVersionMatrixScenario() Scenario {
	return Scenario{
		Name: ScenarioBoltVersionMatrix,
		Description: "Bolt protocol version matrix: 4.4, 5.0, 5.1 and 5.6 negotiated and driven side by side — " +
			"the negotiated version must be a function of what the client offered and not of how it spelled it, " +
			"a wrong password on HELLO must be refused at 4.4/5.0 and IGNORED at 5.1+, entity and zoned-datetime " +
			"encodings must differ across the major boundary exactly where the encoder branches, and the decoded " +
			"semantics — entity cores, parameter round trips, typed refusals and committed writes — must be " +
			"identical at every version",
		Mode:        ModeDeterministic,
		DefaultSeed: boltVersionMatrixDefaultSeed,
		run:         runBoltVersionMatrixScenario,
	}
}

// runBoltVersionMatrixScenario collects the evidence and adjudicates it against
// both the contract and the non-vacuity gate.
func runBoltVersionMatrixScenario(ctx context.Context, seed uint64) (*SimReport, error) {
	ev, err := RunBoltVersionMatrix(ctx, seed)
	if err != nil {
		return nil, err
	}
	v := append(checkBoltVersionMatrix(ev), checkBoltVersionMatrixNonVacuity(ev)...)
	if len(v) == 0 {
		return nil, nil
	}
	return &SimReport{
		Scenario:   ScenarioBoltVersionMatrix,
		Mode:       ModeDeterministic,
		Seed:       seed,
		FailedOp:   Op{Kind: OpMatch, Cypher: "<bolt protocol version matrix>"},
		Violations: v,
	}, nil
}
