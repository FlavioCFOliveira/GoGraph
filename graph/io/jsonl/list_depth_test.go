package jsonl

// list_depth_test.go — regression gate for #1888: nested "list" property values
// must be bounded by an explicit recursion-depth cap (maxListNestingDepth), not
// only by the implicit ~1.8x-per-level JSON-escaping growth against the byte
// cap. Building a real payload nested to the cap is infeasible (the encoded
// size grows exponentially — that growth is exactly the implicit bound this cap
// backstops), so the guard boundary is exercised directly via the internal
// depth-tracked decoder, and a modest nesting is exercised end-to-end.

import (
	"encoding/json"
	"errors"
	"testing"
)

// buildNestedList returns a (kind, value) encoding a "list" property nested
// depth levels deep, wrapping an innermost int64. Each level is a JSON array of
// one [kind, encodedValue] pair — the exact shape decodePropertyValue parses.
// Keep depth small: the encoded size grows exponentially per level.
func buildNestedList(t *testing.T, depth int) (string, string) {
	t.Helper()
	kind, value := "int64", "1"
	for i := 0; i < depth; i++ {
		inner, err := json.Marshal([][2]string{{kind, value}})
		if err != nil {
			t.Fatalf("marshal level %d: %v", i, err)
		}
		kind, value = "list", string(inner)
	}
	return kind, value
}

func TestDecodePropertyValue_ListDepthCapped(t *testing.T) {
	t.Parallel()

	// At the depth cap the guard fail-stops with ErrListTooDeep. "[]" is a valid
	// empty-list value, so only the depth guard — not a parse error — can fire.
	if _, err := decodePropertyValueDepth("list", "[]", maxListNestingDepth); !errors.Is(err, ErrListTooDeep) {
		t.Fatalf("at depth cap: err = %v, want ErrListTooDeep", err)
	}

	// One level below the cap still decodes (an empty list).
	if _, err := decodePropertyValueDepth("list", "[]", maxListNestingDepth-1); err != nil {
		t.Fatalf("one below cap: unexpected err %v", err)
	}

	// The public entry point starts at depth 0 and decodes a modestly nested
	// list end-to-end.
	kind, value := buildNestedList(t, 6)
	if _, err := decodePropertyValue(kind, value); err != nil {
		t.Fatalf("depth-6 list rejected: %v", err)
	}
}
