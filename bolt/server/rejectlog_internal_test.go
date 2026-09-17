package server

import (
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

// rejectlog_internal_test.go — rmp #2835.
//
// The limiter is tested against a SYNTHETIC clock rather than wall time, which
// is the whole reason allow takes nowNanos as a parameter: the bound it promises
// is exact, so the test that states it should be exact too, and should not be a
// sleep.

// TestRejectLogLimiter_FirstRefusalAlwaysEmits pins the guarantee the task
// states in as many words: whatever the sampling does, the first rejection of a
// burst is logged. A zero lastNanos is what delivers it, so a change to the
// zero value breaks here rather than in production.
func TestRejectLogLimiter_FirstRefusalAlwaysEmits(t *testing.T) {
	var l rejectLogLimiter
	emit, suppressed := l.allow(time.Now().UnixNano(), time.Second)
	if !emit {
		t.Fatal("the first refusal was suppressed; the first line of a burst must always be written")
	}
	if suppressed != 0 {
		t.Fatalf("first line reported %d suppressed refusals, want 0", suppressed)
	}
}

// TestRejectLogLimiter_BoundsLinesPerInterval is the bound itself: a flood of
// any size inside one interval yields exactly one line, and the line that opens
// the next interval accounts for every refusal swallowed in between.
//
// The two arms differ ONLY in the number of refusals. Equal line counts across a
// 100x difference in rate is the property the task requires — bounded, and not
// growing linearly with the rejection count.
func TestRejectLogLimiter_BoundsLinesPerInterval(t *testing.T) {
	const every = time.Second

	for _, refusals := range []int{100, 10_000} {
		var l rejectLogLimiter
		base := time.Now().UnixNano()

		lines, reported := 0, uint64(0)
		for i := 0; i < refusals; i++ {
			// Every refusal lands inside the SAME interval: the flood is faster
			// than the bound, which is the case the bound exists for.
			emit, suppressed := l.allow(base+int64(i), every)
			if emit {
				lines++
				reported += suppressed
			}
		}
		if lines != 1 {
			t.Errorf("%d refusals in one interval produced %d lines, want exactly 1", refusals, lines)
		}

		// One interval later, the next refusal reports everything swallowed since
		// that first line — so the log's own accounting loses nothing either.
		emit, suppressed := l.allow(base+int64(every)+int64(refusals), every)
		if !emit {
			t.Fatalf("%d refusals: the line opening the next interval was suppressed", refusals)
		}
		lines++
		reported += suppressed

		// The accounting invariant: every refusal offered to the limiter is
		// either written on its own line or counted as suppressed by a later
		// one. Nothing falls between the two.
		offered := uint64(refusals + 1) // the flood, plus the one that opened the next interval
		if reported+uint64(lines) != offered {
			t.Errorf("%d refusals: %d written + %d reported as suppressed = %d, want %d — a refusal was lost from the log's own accounting",
				refusals, lines, reported, reported+uint64(lines), offered)
		}
		t.Logf("%d refusals offered -> %d log lines, %d accounted as suppressed", offered, lines, reported)
	}
}

// benchRejectConn is a real loopback TCP connection, so RemoteAddr().String()
// in the benchmark below costs what it costs in production rather than what a
// stub makes it cost.
func benchRejectConn(b *testing.B) net.Conn {
	b.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("listen: %v", err)
	}
	b.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	b.Cleanup(func() { _ = c.Close() })
	if peer := <-accepted; peer != nil {
		b.Cleanup(func() { _ = peer.Close() })
	}
	return c
}

// BenchmarkRejectBranch measures what one refused connection costs the accept
// goroutine, in the three shapes that matter. All three run in ONE binary on one
// host, so the comparison is interleaved by construction and needs no
// cross-build benchstat to be sound.
//
//	unbounded  — the shape before rmp #2835: resolve the peer address eagerly,
//	             format and write a WARN for every refusal.
//	bounded    — the shape after: the level test, then the rate limiter, and the
//	             address resolved only for the line that is actually written.
//	level-off  — the same code with the logger at LevelError, which is what the
//	             laziness claim is about: nothing of the record is built.
//
// The log sink is a real TextHandler over io.Discard, so the formatting cost is
// real and only the disk is removed.
func BenchmarkRejectBranch(b *testing.B) {
	conn := benchRejectConn(b)
	warn := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn}))
	quiet := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	ctx := b.Context()

	b.Run("unbounded", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			incCounter(metricConnRejected)
			warn.Warn("bolt: max connections reached, rejecting", slog.String("remote", conn.RemoteAddr().String()))
		}
	})

	b.Run("bounded", func(b *testing.B) {
		var l rejectLogLimiter
		b.ReportAllocs()
		for b.Loop() {
			incCounter(metricConnRejected)
			if warn.Enabled(ctx, slog.LevelWarn) {
				if emit, suppressed := l.allow(time.Now().UnixNano(), rejectLogInterval); emit {
					warn.Warn("bolt: max connections reached, rejecting (log rate-limited)",
						slog.String("remote", conn.RemoteAddr().String()),
						slog.Uint64("suppressed_since_last_line", suppressed),
						slog.Duration("log_interval", rejectLogInterval))
				}
			}
		}
	})

	b.Run("bounded-level-off", func(b *testing.B) {
		var l rejectLogLimiter
		b.ReportAllocs()
		for b.Loop() {
			incCounter(metricConnRejected)
			if quiet.Enabled(ctx, slog.LevelWarn) {
				if emit, suppressed := l.allow(time.Now().UnixNano(), rejectLogInterval); emit {
					quiet.Warn("bolt: max connections reached, rejecting (log rate-limited)",
						slog.String("remote", conn.RemoteAddr().String()),
						slog.Uint64("suppressed_since_last_line", suppressed),
						slog.Duration("log_interval", rejectLogInterval))
				}
			}
		}
	})
}
