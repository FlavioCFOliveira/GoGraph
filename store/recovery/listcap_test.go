package recovery

import (
	"encoding/binary"
	"testing"

	"gograph/graph/lpg"
)

// TestRecoveryListCapHint_ClampsHostileCount confirms the capacity hint is
// bounded by the remaining bytes, never by the untrusted count. Finding I3.
func TestRecoveryListCapHint_ClampsHostileCount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		count     uint32
		remaining int
		want      int
	}{
		{"hostile count, no body", 1 << 31, 0, 0},
		{"hostile count, tiny body", 0xFFFFFFFF, 12, 12 / recoveryListElemMinBytes},
		{"legit count below ceiling", 3, 1000, 3},
		{"count equals ceiling", 4, 4 * recoveryListElemMinBytes, 4},
		{"count just above ceiling", 5, 4 * recoveryListElemMinBytes, 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := recoveryListCapHint(tc.count, tc.remaining); got != tc.want {
				t.Fatalf("recoveryListCapHint(%d, %d) = %d, want %d",
					tc.count, tc.remaining, got, tc.want)
			}
		})
	}
}

// TestDecodeRecoveryListProp_HugeCountBounded crafts a PropList blob that
// declares ~4.3e9 elements but carries no element body. The capacity hint
// the decoder uses must be clamped to what the (empty) body could hold —
// not to the hostile count — and the decode must return a truncation error
// rather than eagerly reserving gigabytes. The clamp is asserted directly
// via recoveryListCapHint so the check is deterministic (a process-global
// allocation delta is unreliable under parallel -race runs). Finding I3.
func TestDecodeRecoveryListProp_HugeCountBounded(t *testing.T) {
	t.Parallel()
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, 0xFFFFFFFF) // ~4.3e9 declared elements
	body := buf[4:]

	if hint := recoveryListCapHint(0xFFFFFFFF, len(body)); hint != 0 {
		t.Fatalf("recoveryListCapHint(huge, %d) = %d, want 0 (no eager reservation)", len(body), hint)
	}

	if _, _, err := decodeRecoveryListProp(buf); err == nil {
		t.Fatal("decodeRecoveryListProp(huge count, empty body) = nil error, want truncation error")
	}
}

// TestDecodeRecoveryListProp_ValidRoundTrip confirms a legitimate list still
// decodes to the exact element set after the capacity-hint change. The blob
// is hand-built in the on-wire format the decoder expects (count, then
// per-element kind|payloadLen|payload). Finding I3.
func TestDecodeRecoveryListProp_ValidRoundTrip(t *testing.T) {
	t.Parallel()
	// Two PropString elements: "alpha" and "beta".
	want := []string{"alpha", "beta"}
	var buf []byte
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(want)))
	for _, s := range want {
		buf = append(buf, byte(lpg.PropString))
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(s)))
		buf = append(buf, s...)
	}

	val, rest, err := decodeRecoveryListProp(buf)
	if err != nil {
		t.Fatalf("decodeRecoveryListProp: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("trailing bytes after decode = %d, want 0", len(rest))
	}
	elems, ok := val.List()
	if !ok {
		t.Fatal("decoded value is not a list")
	}
	if len(elems) != len(want) {
		t.Fatalf("decoded list len = %d, want %d", len(elems), len(want))
	}
	for i, w := range want {
		got, ok := elems[i].String()
		if !ok || got != w {
			t.Errorf("elem[%d] = %q (%v), want %q", i, got, ok, w)
		}
	}
}
