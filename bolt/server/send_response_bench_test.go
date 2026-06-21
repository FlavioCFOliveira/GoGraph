package server

// send_response_bench_test.go — empirical evidence for #1518.
//
// Every Bolt response frame (SUCCESS / RECORD / FAILURE) is written by
// sendResponse. Before #1518 it allocated a fresh bytes.Buffer, a
// packstream.Encoder, and the encoder's internal bufio.Writer on every call —
// so a streaming PULL of N rows paid that allocation set N times. After, the
// buffer+encoder are reused from respBufPool. These benchmarks isolate the
// per-message cost over a representative RECORD row and a SUCCESS metadata
// frame. Layer: short.
//
// Run with:
//
//	go test -run=^$ -bench=BenchmarkSendResponse -benchmem -count=6 ./bolt/server/

import (
	"io"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	proto "github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// BenchmarkSendResponse_Record measures the response-encode path for a typical
// query row (the dominant message type during a PULL).
func BenchmarkSendResponse_Record(b *testing.B) {
	cw := proto.NewChunkedWriter(io.Discard)
	rec := &proto.Record{Data: []packstream.Value{int64(42), "alice@example.com", 3.14159, true, "2026-06-22"}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := sendResponse(cw, rec); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSendResponse_Success measures the response-encode path for a SUCCESS
// metadata frame (sent after RUN and at PULL completion).
func BenchmarkSendResponse_Success(b *testing.B) {
	cw := proto.NewChunkedWriter(io.Discard)
	succ := &proto.Success{Metadata: map[string]packstream.Value{
		"fields":  []packstream.Value{"id", "email"},
		"t_first": int64(1),
	}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := sendResponse(cw, succ); err != nil {
			b.Fatal(err)
		}
	}
}
