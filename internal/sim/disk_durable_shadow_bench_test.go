package sim

import (
	"fmt"
	"os"
	"testing"
)

// BenchmarkSimDiskDurableShadow_WALAppendSync measures the cost the durable
// shadow adds to the access pattern that dominates the harness: the WAL's
// open-once, append-then-fsync loop (rmp #2535).
//
// It is the evidence for choosing the O(1) durableLen WATERMARK plus
// copy-on-write over an unconditional durableData clone. The clone costs O(len)
// per fsync, so a file of n bytes hardened over m fsyncs copies O(n*m) bytes —
// quadratic in the frame count for an append-only log, which is exactly the WAL.
// The watermark copies nothing unless a write reaches back below it, which the
// WAL never does.
//
// Sweeping the frame count is the point: a single size cannot distinguish a
// constant overhead from a growing one. Run it as
//
//	go test -run XXX -bench BenchmarkSimDiskDurableShadow -benchmem -count=10 ./internal/sim/
//
// and compare arms with benchstat.
//
// # Measured (Apple M4, darwin/arm64, count=6, benchstat)
//
// Against the pre-#2535 tree, which had no durable shadow at all, the watermark
// is not distinguishable at any size — so the fidelity fix is free on the hot
// pattern:
//
//	frames=256    56.97µ -> 58.46µ  ~ (p=0.093)   B/op +0.01%   allocs +0.00%
//	frames=1024   706.4µ -> 709.5µ  ~ (p=0.699)   B/op +0.00%   allocs +0.00%
//	frames=4096   9.997m -> 10.003m ~ (p=0.818)   B/op    ~     allocs    ~
//
// Against the same tree with markSyncedTo replaced by an unconditional
// durableData clone, the cost is a clean doubling of the entire append path,
// flat across sizes because SimDisk's own growing Write already reallocates the
// whole buffer per append — the clone simply copies every one of those bytes a
// second time:
//
//	geomean  sec/op +97.64%   B/op +99.94%   allocs/op +97.84%   (p=0.002, all sizes)
//
// Hence the watermark. The copy-on-write path it falls back to is reached only
// by a write below the durable length, which the WAL never performs.
func BenchmarkSimDiskDurableShadow_WALAppendSync(b *testing.B) {
	frame := make([]byte, 16)
	for i := range frame {
		frame[i] = 0xA5
	}
	for _, frames := range []int{256, 1024, 4096} {
		b.Run(fmt.Sprintf("frames=%d", frames), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				d := NewSimDisk(NewSeed(uint64(i)+1), 0)
				h, err := d.OpenFile("wal", os.O_CREATE|os.O_WRONLY|os.O_APPEND)
				if err != nil {
					b.Fatalf("OpenFile: %v", err)
				}
				for f := 0; f < frames; f++ {
					if _, err := h.Write(frame); err != nil {
						b.Fatalf("Write: %v", err)
					}
					if err := h.Sync(); err != nil {
						b.Fatalf("Sync: %v", err)
					}
				}
				if err := h.Close(); err != nil {
					b.Fatalf("Close: %v", err)
				}
			}
		})
	}
}
