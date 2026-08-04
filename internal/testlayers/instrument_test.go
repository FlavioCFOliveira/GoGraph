package testlayers

// instrument_test.go — rmp #2319: the coverage precondition behaves, and the skip
// is never silent.
//
// Layer: short.

import (
	"fmt"
	"strings"
	"testing"
)

// fakeTB records what a skip said, so the LOUDNESS requirement is checkable rather
// than merely intended. It implements the sliver of testing.TB
// [RequireUninstrumented] uses.
type fakeTB struct {
	testing.TB
	skipped bool
	msg     string
}

func (f *fakeTB) Helper() {}

func (f *fakeTB) Skipf(format string, args ...any) {
	f.skipped = true
	f.msg = fmt.Sprintf(format, args...)
}

// TestRequireUninstrumented_SkipsOnlyWhenInstrumented pins both directions.
func TestRequireUninstrumented_SkipsOnlyWhenInstrumented(t *testing.T) {
	orig := coverModeFn
	t.Cleanup(func() { coverModeFn = orig })

	// UNINSTRUMENTED: the control must assert, because that is the arm in which a
	// genuinely broken control has to be caught.
	coverModeFn = func() string { return "" }
	f := &fakeTB{}
	RequireUninstrumented(f, "the quantity under test")
	if f.skipped {
		t.Errorf("an uninstrumented build skipped the control: %s", f.msg)
	}
	if Instrumented() {
		t.Error("Instrumented() is true with an empty cover mode")
	}

	// INSTRUMENTED: it must skip, and the message must name BOTH the mode and the
	// caller's measurement — a skip that says only "skipped" is the failure mode this
	// test exists for, because a silently-skipped gate reads as a passing one.
	coverModeFn = func() string { return "set" }
	f = &fakeTB{}
	RequireUninstrumented(f, "the serialisation ratio of parallel work through one mutex")
	if !f.skipped {
		t.Fatal("an instrumented build did not skip the control")
	}
	for _, want := range []string{
		`CoverMode="set"`,
		"the serialisation ratio of parallel work through one mutex",
		"Nothing is concluded about the module",
		"rmp #2319",
	} {
		if !strings.Contains(f.msg, want) {
			t.Errorf("the skip message does not mention %q; it said: %s", want, f.msg)
		}
	}
	if !Instrumented() {
		t.Error("Instrumented() is false with a non-empty cover mode")
	}
}
