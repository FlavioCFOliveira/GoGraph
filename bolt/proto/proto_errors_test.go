package proto_test

import (
	"bytes"
	"testing"

	"gograph/bolt/packstream"
	"gograph/bolt/proto"
)

// helper: encode a struct header (tag, n fields) and optional extra bytes.
func structBytes(tag byte, n int, extra ...byte) []byte {
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	_ = enc.WriteStructHeader(tag, n)
	_ = enc.Flush()
	b := buf.Bytes()
	return append(b, extra...)
}

// ─────────────────────────────────────────────────────────────────────────────
// EncodeRequest / EncodeResponse unknown-type errors
// ─────────────────────────────────────────────────────────────────────────────

func TestEncodeRequest_UnknownType(t *testing.T) {
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := proto.EncodeRequest(enc, "not-a-bolt-message"); err == nil {
		t.Fatal("expected error for unknown request type")
	}
}

func TestEncodeResponse_UnknownType(t *testing.T) {
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := proto.EncodeResponse(enc, "not-a-bolt-message"); err == nil {
		t.Fatal("expected error for unknown response type")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DecodeRequest / DecodeResponse unknown-tag errors
// ─────────────────────────────────────────────────────────────────────────────

func TestDecodeRequest_UnknownTag(t *testing.T) {
	// Struct with tag 0xAA (not a known Bolt request tag).
	raw := structBytes(0xAA, 0)
	dec := packstream.NewDecoder(bytes.NewReader(raw))
	_, err := proto.DecodeRequest(dec)
	if err == nil {
		t.Fatal("expected error for unknown request tag")
	}
}

func TestDecodeResponse_UnknownTag(t *testing.T) {
	// Struct with tag 0xAA (not a known Bolt response tag).
	raw := structBytes(0xAA, 0)
	dec := packstream.NewDecoder(bytes.NewReader(raw))
	_, err := proto.DecodeResponse(dec)
	if err == nil {
		t.Fatal("expected error for unknown response tag")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Wrong field count errors
// ─────────────────────────────────────────────────────────────────────────────

func TestDecodeRequest_HelloWrongFieldCount(t *testing.T) {
	raw := structBytes(proto.TagHello, 0) // Hello expects 1 field
	dec := packstream.NewDecoder(bytes.NewReader(raw))
	_, err := proto.DecodeRequest(dec)
	if err == nil {
		t.Fatal("expected error for Hello with 0 fields")
	}
}

func TestDecodeRequest_LogonWrongFieldCount(t *testing.T) {
	raw := structBytes(proto.TagLogon, 0) // Logon expects 1 field
	dec := packstream.NewDecoder(bytes.NewReader(raw))
	_, err := proto.DecodeRequest(dec)
	if err == nil {
		t.Fatal("expected error for Logon with 0 fields")
	}
}

func TestDecodeRequest_RunWrongFieldCount(t *testing.T) {
	raw := structBytes(proto.TagRun, 0) // Run expects 3 fields
	dec := packstream.NewDecoder(bytes.NewReader(raw))
	_, err := proto.DecodeRequest(dec)
	if err == nil {
		t.Fatal("expected error for Run with 0 fields")
	}
}

func TestDecodeRequest_BeginWrongFieldCount(t *testing.T) {
	raw := structBytes(proto.TagBegin, 0) // Begin expects 1 field
	dec := packstream.NewDecoder(bytes.NewReader(raw))
	_, err := proto.DecodeRequest(dec)
	if err == nil {
		t.Fatal("expected error for Begin with 0 fields")
	}
}

func TestDecodeRequest_RouteWrongFieldCount(t *testing.T) {
	raw := structBytes(proto.TagRoute, 0) // Route expects 3 fields
	dec := packstream.NewDecoder(bytes.NewReader(raw))
	_, err := proto.DecodeRequest(dec)
	if err == nil {
		t.Fatal("expected error for Route with 0 fields")
	}
}

func TestDecodeResponse_SuccessWrongFieldCount(t *testing.T) {
	raw := structBytes(proto.TagSuccess, 0) // Success expects 1 field
	dec := packstream.NewDecoder(bytes.NewReader(raw))
	_, err := proto.DecodeResponse(dec)
	if err == nil {
		t.Fatal("expected error for Success with 0 fields")
	}
}

func TestDecodeResponse_FailureWrongFieldCount(t *testing.T) {
	raw := structBytes(proto.TagFailure, 0) // Failure expects 1 field
	dec := packstream.NewDecoder(bytes.NewReader(raw))
	_, err := proto.DecodeResponse(dec)
	if err == nil {
		t.Fatal("expected error for Failure with 0 fields")
	}
}

func TestDecodeResponse_RecordWrongFieldCount(t *testing.T) {
	raw := structBytes(proto.TagRecord, 0) // Record expects 1 field
	dec := packstream.NewDecoder(bytes.NewReader(raw))
	_, err := proto.DecodeResponse(dec)
	if err == nil {
		t.Fatal("expected error for Record with 0 fields")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// readMap nil and type-assertion paths
// ─────────────────────────────────────────────────────────────────────────────

func TestDecodeRequest_HelloWithNullMap(t *testing.T) {
	// Hello with 1 field = null — readMap returns nil, nil → Hello{Extra: nil}.
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	_ = enc.WriteStructHeader(proto.TagHello, 1)
	_ = enc.WriteNull() // null instead of a map
	_ = enc.Flush()
	dec := packstream.NewDecoder(bytes.NewReader(buf.Bytes()))
	msg, err := proto.DecodeRequest(dec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	h, ok := msg.(*proto.Hello)
	if !ok {
		t.Fatalf("expected *Hello, got %T", msg)
	}
	if h.Extra != nil {
		t.Errorf("expected nil Extra, got %v", h.Extra)
	}
}

func TestDecodeRequest_HelloWithNonMap(t *testing.T) {
	// Hello with 1 field = string — readMap type assertion fails.
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	_ = enc.WriteStructHeader(proto.TagHello, 1)
	_ = enc.WriteString("notamap")
	_ = enc.Flush()
	dec := packstream.NewDecoder(bytes.NewReader(buf.Bytes()))
	_, err := proto.DecodeRequest(dec)
	if err == nil {
		t.Fatal("expected error for Hello with string instead of map")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// decodeRecord: nil list and non-list paths
// ─────────────────────────────────────────────────────────────────────────────

func TestDecodeResponse_RecordNullData(t *testing.T) {
	// Record with 1 field = null → Record{Data: nil}.
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	_ = enc.WriteStructHeader(proto.TagRecord, 1)
	_ = enc.WriteNull()
	_ = enc.Flush()
	dec := packstream.NewDecoder(bytes.NewReader(buf.Bytes()))
	msg, err := proto.DecodeResponse(dec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rec, ok := msg.(*proto.Record)
	if !ok {
		t.Fatalf("expected *Record, got %T", msg)
	}
	if rec.Data != nil {
		t.Errorf("expected nil Data, got %v", rec.Data)
	}
}

func TestDecodeResponse_RecordNonList(t *testing.T) {
	// Record with 1 field = string → type-assertion failure.
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	_ = enc.WriteStructHeader(proto.TagRecord, 1)
	_ = enc.WriteString("notalist")
	_ = enc.Flush()
	dec := packstream.NewDecoder(bytes.NewReader(buf.Bytes()))
	_, err := proto.DecodeResponse(dec)
	if err == nil {
		t.Fatal("expected error for Record with string instead of list")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// skipFields: Logoff/Goodbye/Reset/Commit/Rollback with n=0 (exercise skip)
// ─────────────────────────────────────────────────────────────────────────────

func TestDecodeRequest_LogoffSkipFields(t *testing.T) {
	raw := structBytes(proto.TagLogoff, 0)
	dec := packstream.NewDecoder(bytes.NewReader(raw))
	msg, err := proto.DecodeRequest(dec)
	if err != nil {
		t.Fatalf("Logoff: %v", err)
	}
	if _, ok := msg.(*proto.Logoff); !ok {
		t.Fatalf("expected *Logoff, got %T", msg)
	}
}

func TestDecodeRequest_CommitSkipFields(t *testing.T) {
	raw := structBytes(proto.TagCommit, 0)
	dec := packstream.NewDecoder(bytes.NewReader(raw))
	msg, err := proto.DecodeRequest(dec)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, ok := msg.(*proto.Commit); !ok {
		t.Fatalf("expected *Commit, got %T", msg)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// decodeRoute: non-list bookmarks path
// ─────────────────────────────────────────────────────────────────────────────

func TestDecodeRequest_RouteNonListBookmarks(t *testing.T) {
	// Route with 3 fields: routing={}, bookmarks=string (wrong type), db="neo4j"
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	_ = enc.WriteStructHeader(proto.TagRoute, 3)
	_ = enc.WriteValue(packstream.Value(map[string]packstream.Value{})) // routing
	_ = enc.WriteString("notalist")                                     // bookmarks — wrong type
	_ = enc.WriteString("neo4j")                                        // db
	_ = enc.Flush()
	dec := packstream.NewDecoder(bytes.NewReader(buf.Bytes()))
	_, err := proto.DecodeRequest(dec)
	if err == nil {
		t.Fatal("expected error for Route with string bookmarks")
	}
}

func TestDecodeRequest_RouteNullBookmarks(t *testing.T) {
	// Route with null bookmarks — valid, bookmarks = nil.
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	_ = enc.WriteStructHeader(proto.TagRoute, 3)
	_ = enc.WriteValue(packstream.Value(map[string]packstream.Value{}))
	_ = enc.WriteNull()
	_ = enc.WriteString("neo4j")
	_ = enc.Flush()
	dec := packstream.NewDecoder(bytes.NewReader(buf.Bytes()))
	msg, err := proto.DecodeRequest(dec)
	if err != nil {
		t.Fatalf("Route with null bookmarks: %v", err)
	}
	r, ok := msg.(*proto.Route)
	if !ok {
		t.Fatalf("expected *Route, got %T", msg)
	}
	if r.Bookmarks != nil {
		t.Errorf("expected nil Bookmarks, got %v", r.Bookmarks)
	}
}
