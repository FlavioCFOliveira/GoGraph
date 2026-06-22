package proto_test

import (
	"bytes"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	proto "github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

func mustEnc(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
}

// TestPullDiscard_StreamingExtraConsumesAllEntries is the #1522 gate. The
// streaming extra-map decode reads only n and qid but must consume EVERY entry
// (including unknown keys and nested values) so the stream stays aligned for the
// next message. A miscount would desync the decoder, so this encodes a PULL with
// a rich extra map followed by a DISCARD and asserts both decode correctly.
func TestPullDiscard_StreamingExtraConsumesAllEntries(t *testing.T) {
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)

	// PULL { n: 100, db: "neo4j", qid: 7, flags: [1, 2] } — n/qid interleaved
	// with unknown keys, one carrying a nested list value.
	mustEnc(t, enc.WriteStructHeader(proto.TagPull, 1))
	mustEnc(t, enc.WriteMapHeader(4))
	mustEnc(t, enc.WriteString("n"))
	mustEnc(t, enc.WriteInt(100))
	mustEnc(t, enc.WriteString("db"))
	mustEnc(t, enc.WriteString("neo4j"))
	mustEnc(t, enc.WriteString("qid"))
	mustEnc(t, enc.WriteInt(7))
	mustEnc(t, enc.WriteString("flags"))
	mustEnc(t, enc.WriteListHeader(2))
	mustEnc(t, enc.WriteInt(1))
	mustEnc(t, enc.WriteInt(2))

	// Trailing DISCARD { } — only decodes correctly if the PULL above consumed
	// all four extra entries and left the stream aligned.
	mustEnc(t, enc.WriteStructHeader(proto.TagDiscard, 1))
	mustEnc(t, enc.WriteMapHeader(0))
	mustEnc(t, enc.Flush())

	dec := packstream.NewDecoder(bytes.NewReader(buf.Bytes()))

	m1, err := proto.DecodeRequest(dec)
	if err != nil {
		t.Fatalf("decode PULL: %v", err)
	}
	pull, ok := m1.(*proto.Pull)
	if !ok {
		t.Fatalf("want *Pull, got %T", m1)
	}
	if pull.N != 100 || pull.QID != 7 {
		t.Errorf("PULL extra: want N=100 QID=7, got N=%d QID=%d", pull.N, pull.QID)
	}

	m2, err := proto.DecodeRequest(dec)
	if err != nil {
		t.Fatalf("decode trailing DISCARD failed — PULL extra decode desynced the stream: %v", err)
	}
	disc, ok := m2.(*proto.Discard)
	if !ok {
		t.Fatalf("want *Discard after PULL, got %T (stream desynced)", m2)
	}
	if disc.N != -1 || disc.QID != -1 {
		t.Errorf("empty-extra DISCARD: want N=-1 QID=-1, got N=%d QID=%d", disc.N, disc.QID)
	}
}

// TestPull_NullExtraToleratedAsEmpty asserts a null (non-map) extra is tolerated
// as empty — preserving the previous readMap behaviour (n=-1, qid=-1).
func TestPull_NullExtraToleratedAsEmpty(t *testing.T) {
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	mustEnc(t, enc.WriteStructHeader(proto.TagPull, 1))
	mustEnc(t, enc.WriteNull())
	mustEnc(t, enc.Flush())

	dec := packstream.NewDecoder(bytes.NewReader(buf.Bytes()))
	m, err := proto.DecodeRequest(dec)
	if err != nil {
		t.Fatalf("decode PULL with null extra: %v", err)
	}
	pull, ok := m.(*proto.Pull)
	if !ok {
		t.Fatalf("want *Pull, got %T", m)
	}
	if pull.N != -1 || pull.QID != -1 {
		t.Errorf("null extra: want N=-1 QID=-1, got N=%d QID=%d", pull.N, pull.QID)
	}
}
