package snapshot

import (
	"encoding/binary"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestListCapHint_ClampsHostileCount confirms the capacity hint is bounded
// by the bytes that remain, never by the untrusted count. Finding I3.
func TestListCapHint_ClampsHostileCount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		count     uint32
		remaining int
		want      int
	}{
		{"hostile count, no body", 1 << 31, 0, 0},
		{"hostile count, tiny body", 0xFFFFFFFF, 12, 12 / listElemMinBytes},
		{"legit count below ceiling", 3, 1000, 3},
		{"count equals ceiling", 4, 4 * listElemMinBytes, 4},
		{"count just above ceiling", 5, 4 * listElemMinBytes, 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := listCapHint(tc.count, tc.remaining); got != tc.want {
				t.Fatalf("listCapHint(%d, %d) = %d, want %d", tc.count, tc.remaining, got, tc.want)
			}
		})
	}
}

// TestDecodeListPropertyValue_HugeCountBounded crafts a PropList blob that
// declares ~4.3e9 elements but carries no element body. The capacity hint
// the decoder uses must be clamped to what the (empty) body could hold —
// not to the hostile count — and the decode must return a truncation error
// rather than eagerly reserving gigabytes. The clamp is asserted directly
// via listCapHint so the check is deterministic (a process-global
// allocation delta is unreliable under parallel -race runs). Finding I3.
func TestDecodeListPropertyValue_HugeCountBounded(t *testing.T) {
	t.Parallel()
	raw := make([]byte, 4)
	binary.LittleEndian.PutUint32(raw, 0xFFFFFFFF) // ~4.3e9 declared elements
	body := raw[4:]

	// The decoder sizes its buffer with listCapHint(count, len(body)); for an
	// empty body that is 0, never the 4.3e9 count.
	if hint := listCapHint(0xFFFFFFFF, len(body)); hint != 0 {
		t.Fatalf("listCapHint(huge, %d) = %d, want 0 (no eager reservation)", len(body), hint)
	}

	// And the decode itself fails fast on the truncated body.
	if _, err := decodeListPropertyValue(raw); err == nil {
		t.Fatal("decodeListPropertyValue(huge count, empty body) = nil error, want truncation error")
	}
}

// TestDecodeListPropertyValue_ValidRoundTrip confirms a legitimate list
// still decodes to the exact element set after the capacity-hint change.
// Finding I3.
func TestDecodeListPropertyValue_ValidRoundTrip(t *testing.T) {
	t.Parallel()
	orig := lpg.ListValue([]lpg.PropertyValue{
		lpg.Int64Value(1),
		lpg.Int64Value(2),
		lpg.StringValue("three"),
		lpg.BoolValue(true),
	})
	enc, err := encodeListPropertyValue(orig)
	if err != nil {
		t.Fatalf("encodeListPropertyValue: %v", err)
	}
	got, err := decodeListPropertyValue(enc)
	if err != nil {
		t.Fatalf("decodeListPropertyValue: %v", err)
	}
	gotList, ok := got.List()
	if !ok {
		t.Fatal("decoded value is not a list")
	}
	if len(gotList) != 4 {
		t.Fatalf("decoded list len = %d, want 4", len(gotList))
	}
	if v, ok := gotList[0].Int64(); !ok || v != 1 {
		t.Errorf("elem[0] = %v (%v), want 1", v, ok)
	}
	if v, ok := gotList[2].String(); !ok || v != "three" {
		t.Errorf("elem[2] = %q (%v), want \"three\"", v, ok)
	}
	if v, ok := gotList[3].Bool(); !ok || !v {
		t.Errorf("elem[3] = %v (%v), want true", v, ok)
	}
}
