// Package proto implements the Bolt v5 wire protocol message types,
// handshake negotiation, and chunked framing.
//
// Concurrency: all types in this package are NOT safe for concurrent use
// unless documented otherwise. Callers must ensure that a single connection's
// read and write paths are each accessed by at most one goroutine at a time.
package proto

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
)

// Structure tag bytes for Bolt v5 messages (PackStream Struct tags).
const (
	// Client → Server.
	TagHello    byte = 0x01
	TagLogon    byte = 0x6A
	TagLogoff   byte = 0x6B
	TagGoodbye  byte = 0x02
	TagReset    byte = 0x0F
	TagRun      byte = 0x10
	TagPull     byte = 0x3F
	TagDiscard  byte = 0x2F
	TagBegin    byte = 0x11
	TagCommit   byte = 0x12
	TagRollback byte = 0x13
	TagRoute    byte = 0x66

	// Server → Client.
	TagSuccess byte = 0x70
	TagFailure byte = 0x7F
	TagIgnored byte = 0x7E
	TagRecord  byte = 0x71
)

// ---------------------------------------------------------------------------
// Client → Server message structs
// ---------------------------------------------------------------------------

// Hello is sent by the client to initiate a Bolt connection.
// Fields: [extra map] containing agent, bolt_agent, scheme, principal,
// credentials, routing, and other driver metadata.
type Hello struct {
	Extra map[string]packstream.Value
}

// Logon sends authentication credentials on an established connection.
// Fields: [auth map].
type Logon struct {
	Auth map[string]packstream.Value
}

// Logoff ends an authenticated session without closing the connection.
// Fields: none.
type Logoff struct{}

// Goodbye signals orderly connection teardown.
// Fields: none.
type Goodbye struct{}

// Reset returns the connection to a clean, ready state, discarding any
// pending results and rolling back any open transaction.
// Fields: none.
type Reset struct{}

// Run submits a Cypher query for execution.
// Fields: [query string, parameters map, extra map].
type Run struct {
	Query      string
	Parameters map[string]packstream.Value
	Extra      map[string]packstream.Value
}

// Pull requests records from the server.
// Fields: [extra map {n: int64, qid: int64}].
// n=-1 means pull all; qid=-1 means the most recent query.
type Pull struct {
	N   int64
	QID int64
}

// Discard discards pending records without streaming them to the client.
// Fields: [extra map {n: int64, qid: int64}].
type Discard struct {
	N   int64
	QID int64
}

// Begin starts an explicit transaction.
// Fields: [extra map].
type Begin struct {
	Extra map[string]packstream.Value
}

// Commit commits the current explicit transaction.
// Fields: none.
type Commit struct{}

// Rollback rolls back the current explicit transaction.
// Fields: none.
type Rollback struct{}

// Route requests routing table information.
// Fields: [routing map, bookmarks list, db string|null].
type Route struct {
	Routing   map[string]packstream.Value
	Bookmarks []packstream.Value
	DB        packstream.Value // string or nil
}

// ---------------------------------------------------------------------------
// Server → Client message structs
// ---------------------------------------------------------------------------

// Success indicates that the preceding request succeeded.
// Fields: [metadata map].
type Success struct {
	Metadata map[string]packstream.Value
}

// Failure indicates that the preceding request failed.
// Fields: [metadata map {code string, message string}].
type Failure struct {
	Code    string
	Message string
}

// Ignored indicates that the preceding request was ignored (e.g., the
// connection is in a failed state).
// Fields: none.
type Ignored struct{}

// Record carries one row of result data from a query.
// Fields: [data list].
type Record struct {
	Data []packstream.Value
}

// ---------------------------------------------------------------------------
// EncodeRequest / DecodeRequest
// ---------------------------------------------------------------------------

// EncodeRequest encodes a client→server message msg into enc.
// msg must be one of: *Hello, *Logon, *Logoff, *Goodbye, *Reset, *Run,
// *Pull, *Discard, *Begin, *Commit, *Rollback, *Route.
func EncodeRequest(enc *packstream.Encoder, msg any) error {
	switch m := msg.(type) {
	case *Hello:
		return encodeHello(enc, m)
	case *Logon:
		return encodeLogon(enc, m)
	case *Logoff:
		return encodeEmpty(enc, TagLogoff)
	case *Goodbye:
		return encodeEmpty(enc, TagGoodbye)
	case *Reset:
		return encodeEmpty(enc, TagReset)
	case *Run:
		return encodeRun(enc, m)
	case *Pull:
		return encodePullDiscard(enc, TagPull, m.N, m.QID)
	case *Discard:
		return encodePullDiscard(enc, TagDiscard, m.N, m.QID)
	case *Begin:
		return encodeBegin(enc, m)
	case *Commit:
		return encodeEmpty(enc, TagCommit)
	case *Rollback:
		return encodeEmpty(enc, TagRollback)
	case *Route:
		return encodeRoute(enc, m)
	default:
		return fmt.Errorf("proto: unknown request message type %T", msg)
	}
}

// DecodeRequest reads one complete Bolt request message from dec and returns
// a typed pointer (*Hello, *Logon, etc.).
func DecodeRequest(dec *packstream.Decoder) (any, error) {
	tag, n, err := dec.ReadStructHeader()
	if err != nil {
		return nil, fmt.Errorf("proto: DecodeRequest struct header: %w", err)
	}
	switch tag {
	case TagHello:
		return decodeHello(dec, n)
	case TagLogon:
		return decodeLogon(dec, n)
	case TagLogoff:
		return &Logoff{}, skipFields(dec, n)
	case TagGoodbye:
		return &Goodbye{}, skipFields(dec, n)
	case TagReset:
		return &Reset{}, skipFields(dec, n)
	case TagRun:
		return decodeRun(dec, n)
	case TagPull:
		return decodePullDiscard[Pull](dec, n)
	case TagDiscard:
		return decodePullDiscard[Discard](dec, n)
	case TagBegin:
		return decodeBegin(dec, n)
	case TagCommit:
		return &Commit{}, skipFields(dec, n)
	case TagRollback:
		return &Rollback{}, skipFields(dec, n)
	case TagRoute:
		return decodeRoute(dec, n)
	default:
		return nil, fmt.Errorf("proto: unknown request tag 0x%02X", tag)
	}
}

// ---------------------------------------------------------------------------
// EncodeResponse / DecodeResponse
// ---------------------------------------------------------------------------

// EncodeResponse encodes a server→client message msg into enc.
// msg must be one of: *Success, *Failure, *Ignored, *Record.
func EncodeResponse(enc *packstream.Encoder, msg any) error {
	switch m := msg.(type) {
	case *Success:
		return encodeSuccess(enc, m)
	case *Failure:
		return encodeFailure(enc, m)
	case *Ignored:
		return encodeEmpty(enc, TagIgnored)
	case *Record:
		return encodeRecord(enc, m)
	default:
		return fmt.Errorf("proto: unknown response message type %T", msg)
	}
}

// DecodeResponse reads one complete Bolt response message from dec and returns
// a typed pointer (*Success, *Failure, *Ignored, *Record).
func DecodeResponse(dec *packstream.Decoder) (any, error) {
	tag, n, err := dec.ReadStructHeader()
	if err != nil {
		return nil, fmt.Errorf("proto: DecodeResponse struct header: %w", err)
	}
	switch tag {
	case TagSuccess:
		return decodeSuccess(dec, n)
	case TagFailure:
		return decodeFailure(dec, n)
	case TagIgnored:
		return &Ignored{}, skipFields(dec, n)
	case TagRecord:
		return decodeRecord(dec, n)
	default:
		return nil, fmt.Errorf("proto: unknown response tag 0x%02X", tag)
	}
}

// ---------------------------------------------------------------------------
// Encode helpers
// ---------------------------------------------------------------------------

func encodeEmpty(enc *packstream.Encoder, tag byte) error {
	return enc.WriteStructHeader(tag, 0)
}

func encodeHello(enc *packstream.Encoder, m *Hello) error {
	if err := enc.WriteStructHeader(TagHello, 1); err != nil {
		return err
	}
	return enc.WriteValue(packstream.Value(m.Extra))
}

func encodeLogon(enc *packstream.Encoder, m *Logon) error {
	if err := enc.WriteStructHeader(TagLogon, 1); err != nil {
		return err
	}
	return enc.WriteValue(packstream.Value(m.Auth))
}

func encodeRun(enc *packstream.Encoder, m *Run) error {
	if err := enc.WriteStructHeader(TagRun, 3); err != nil {
		return err
	}
	if err := enc.WriteString(m.Query); err != nil {
		return err
	}
	if err := enc.WriteValue(packstream.Value(m.Parameters)); err != nil {
		return err
	}
	return enc.WriteValue(packstream.Value(m.Extra))
}

func encodePullDiscard(enc *packstream.Encoder, tag byte, n, qid int64) error {
	if err := enc.WriteStructHeader(tag, 1); err != nil {
		return err
	}
	extra := map[string]packstream.Value{
		"n":   n,
		"qid": qid,
	}
	return enc.WriteValue(packstream.Value(extra))
}

func encodeBegin(enc *packstream.Encoder, m *Begin) error {
	if err := enc.WriteStructHeader(TagBegin, 1); err != nil {
		return err
	}
	return enc.WriteValue(packstream.Value(m.Extra))
}

func encodeRoute(enc *packstream.Encoder, m *Route) error {
	if err := enc.WriteStructHeader(TagRoute, 3); err != nil {
		return err
	}
	if err := enc.WriteValue(packstream.Value(m.Routing)); err != nil {
		return err
	}
	if err := enc.WriteValue(packstream.Value(m.Bookmarks)); err != nil {
		return err
	}
	return enc.WriteValue(m.DB)
}

func encodeSuccess(enc *packstream.Encoder, m *Success) error {
	if err := enc.WriteStructHeader(TagSuccess, 1); err != nil {
		return err
	}
	return enc.WriteValue(packstream.Value(m.Metadata))
}

func encodeFailure(enc *packstream.Encoder, m *Failure) error {
	if err := enc.WriteStructHeader(TagFailure, 1); err != nil {
		return err
	}
	meta := map[string]packstream.Value{
		"code":    m.Code,
		"message": m.Message,
	}
	return enc.WriteValue(packstream.Value(meta))
}

func encodeRecord(enc *packstream.Encoder, m *Record) error {
	if err := enc.WriteStructHeader(TagRecord, 1); err != nil {
		return err
	}
	return enc.WriteValue(packstream.Value(m.Data))
}

// ---------------------------------------------------------------------------
// Decode helpers
// ---------------------------------------------------------------------------

// readMap reads a PackStream map and returns it as map[string]Value.
func readMap(dec *packstream.Decoder) (map[string]packstream.Value, error) {
	v, err := dec.ReadValue()
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]packstream.Value)
	if !ok {
		return nil, fmt.Errorf("proto: expected map, got %T", v)
	}
	return m, nil
}

// skipFields discards n values from the decoder (for unknown/empty messages).
func skipFields(dec *packstream.Decoder, n int) error {
	for range n {
		if _, err := dec.ReadValue(); err != nil {
			return err
		}
	}
	return nil
}

func decodeHello(dec *packstream.Decoder, n int) (*Hello, error) {
	if n != 1 {
		return nil, fmt.Errorf("proto: Hello expects 1 field, got %d", n)
	}
	extra, err := readMap(dec)
	if err != nil {
		return nil, err
	}
	return &Hello{Extra: extra}, nil
}

func decodeLogon(dec *packstream.Decoder, n int) (*Logon, error) {
	if n != 1 {
		return nil, fmt.Errorf("proto: Logon expects 1 field, got %d", n)
	}
	auth, err := readMap(dec)
	if err != nil {
		return nil, err
	}
	return &Logon{Auth: auth}, nil
}

func decodeRun(dec *packstream.Decoder, n int) (*Run, error) {
	if n != 3 {
		return nil, fmt.Errorf("proto: Run expects 3 fields, got %d", n)
	}
	q, err := dec.ReadString()
	if err != nil {
		return nil, err
	}
	params, err := readMap(dec)
	if err != nil {
		return nil, err
	}
	extra, err := readMap(dec)
	if err != nil {
		return nil, err
	}
	return &Run{Query: q, Parameters: params, Extra: extra}, nil
}

// pullDiscardResult is a constraint on the two types decodePullDiscard can
// produce, allowing a single generic implementation.
type pullDiscardResult interface {
	Pull | Discard
}

func decodePullDiscard[T pullDiscardResult](dec *packstream.Decoder, n int) (*T, error) {
	if n != 1 {
		return nil, fmt.Errorf("proto: Pull/Discard expects 1 field, got %d", n)
	}
	// Stream the extra map, reading only n and qid, instead of materialising the
	// whole map[string]Value just to extract two ints (#1522). PULL/DISCARD are
	// the most frequent post-RUN messages, so this drops one map allocation per
	// result page. The decode uses the same ReadMapHeader + per-entry
	// ReadString/ReadValue primitives as the full-map path (packstream
	// readValue), so the byte and decoded-memory budgets — the DoS bound on a
	// hostile extra map — are charged identically; only the unread values are
	// discarded rather than stored.
	var nVal, qidVal int64 = -1, -1
	t, err := dec.PeekType()
	if err != nil {
		return nil, err
	}
	switch t {
	case packstream.TypeNull:
		// A null extra is tolerated as an empty map (matching the previous
		// readMap behaviour): consume it and keep the n=-1/qid=-1 defaults.
		if _, err := dec.ReadValue(); err != nil {
			return nil, err
		}
	case packstream.TypeMap:
		count, err := dec.ReadMapHeader()
		if err != nil {
			return nil, err
		}
		for i := 0; i < count; i++ {
			key, err := dec.ReadString()
			if err != nil {
				return nil, err
			}
			val, err := dec.ReadValue()
			if err != nil {
				return nil, err
			}
			switch key {
			case "n":
				if i64, ok := val.(int64); ok {
					nVal = i64
				}
			case "qid":
				if i64, ok := val.(int64); ok {
					qidVal = i64
				}
			}
		}
	default:
		// A non-map, non-null extra is a protocol violation. Reject it without
		// decoding the bogus value (stricter and cheaper than the previous
		// materialise-then-type-assert path).
		return nil, fmt.Errorf("proto: Pull/Discard extra: expected map, got packstream type %d", t)
	}
	// T is either Pull or Discard — both have the same field layout.
	// We use any conversion to populate the concrete type.
	var result any
	switch any(*new(T)).(type) {
	case Pull:
		result = &Pull{N: nVal, QID: qidVal}
	case Discard:
		result = &Discard{N: nVal, QID: qidVal}
	}
	return result.(*T), nil
}

func decodeBegin(dec *packstream.Decoder, n int) (*Begin, error) {
	if n != 1 {
		return nil, fmt.Errorf("proto: Begin expects 1 field, got %d", n)
	}
	extra, err := readMap(dec)
	if err != nil {
		return nil, err
	}
	return &Begin{Extra: extra}, nil
}

func decodeRoute(dec *packstream.Decoder, n int) (*Route, error) {
	if n != 3 {
		return nil, fmt.Errorf("proto: Route expects 3 fields, got %d", n)
	}
	routing, err := readMap(dec)
	if err != nil {
		return nil, err
	}
	bv, err := dec.ReadValue()
	if err != nil {
		return nil, err
	}
	var bookmarks []packstream.Value
	if bv != nil {
		bl, ok := bv.([]packstream.Value)
		if !ok {
			return nil, fmt.Errorf("proto: Route bookmarks: expected list, got %T", bv)
		}
		bookmarks = bl
	}
	db, err := dec.ReadValue()
	if err != nil {
		return nil, err
	}
	return &Route{Routing: routing, Bookmarks: bookmarks, DB: db}, nil
}

func decodeSuccess(dec *packstream.Decoder, n int) (*Success, error) {
	if n != 1 {
		return nil, fmt.Errorf("proto: Success expects 1 field, got %d", n)
	}
	meta, err := readMap(dec)
	if err != nil {
		return nil, err
	}
	return &Success{Metadata: meta}, nil
}

func decodeFailure(dec *packstream.Decoder, n int) (*Failure, error) {
	if n != 1 {
		return nil, fmt.Errorf("proto: Failure expects 1 field, got %d", n)
	}
	meta, err := readMap(dec)
	if err != nil {
		return nil, err
	}
	var code, message string
	if v, ok := meta["code"]; ok {
		code, _ = v.(string)
	}
	if v, ok := meta["message"]; ok {
		message, _ = v.(string)
	}
	return &Failure{Code: code, Message: message}, nil
}

func decodeRecord(dec *packstream.Decoder, n int) (*Record, error) {
	if n != 1 {
		return nil, fmt.Errorf("proto: Record expects 1 field, got %d", n)
	}
	v, err := dec.ReadValue()
	if err != nil {
		return nil, err
	}
	if v == nil {
		return &Record{Data: nil}, nil
	}
	data, ok := v.([]packstream.Value)
	if !ok {
		return nil, fmt.Errorf("proto: Record data: expected list, got %T", v)
	}
	return &Record{Data: data}, nil
}
