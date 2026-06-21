package server

// req_decode_bench_test.go — empirical evidence for #1517.
//
// The Bolt read path decodes one PackStream request frame per message. Before
// #1517 each decode built a fresh packstream.Decoder (with its ~4 KiB bufio
// buffer) and a bytes.Reader; after, the serve loop reuses a per-connection
// bytes.Reader and a pooled Decoder (reqDecPool). These two benchmarks compare
// the pooled path the serve loop now uses against the old fresh-per-message
// path. Layer: short.
//
// Run with:
//
//	go test -run=^$ -bench=BenchmarkRequestDecode -benchmem -count=6 ./bolt/server/

import (
	"bytes"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	proto "github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// encodeRun returns the PackStream bytes of a representative RUN request.
func encodeRun(b *testing.B) []byte {
	b.Helper()
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	run := &proto.Run{
		Query:      "MATCH (u:USER {id:$id})-[:FRIEND]->(f) RETURN f.name AS name LIMIT 25",
		Parameters: map[string]packstream.Value{"id": "abc123def456abc123def456"},
		Extra:      map[string]packstream.Value{"db": "neo4j"},
	}
	if err := proto.EncodeRequest(enc, run); err != nil {
		b.Fatalf("encode run: %v", err)
	}
	if err := enc.Flush(); err != nil {
		b.Fatalf("flush: %v", err)
	}
	return buf.Bytes()
}

// BenchmarkRequestDecode_Pooled measures the #1517 read path: the pooled Decoder
// reset to a reused bytes.Reader, exactly as the serve loop now does it.
func BenchmarkRequestDecode_Pooled(b *testing.B) {
	raw := encodeRun(b)
	var rdr bytes.Reader
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rdr.Reset(raw)
		dec := reqDecPool.Get(&rdr)
		if _, err := proto.DecodeRequest(dec); err != nil {
			b.Fatal(err)
		}
		reqDecPool.Put(dec)
	}
}

// BenchmarkRequestDecode_Fresh measures the pre-#1517 path: a fresh Decoder and
// bytes.Reader allocated per message.
func BenchmarkRequestDecode_Fresh(b *testing.B) {
	raw := encodeRun(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dec := packstream.NewDecoder(bytes.NewReader(raw))
		if _, err := proto.DecodeRequest(dec); err != nil {
			b.Fatal(err)
		}
	}
}
