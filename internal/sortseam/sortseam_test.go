package sortseam

import "testing"

// TestSetKeyDecorationDisabledRestores pins the two properties every caller of
// the seam relies on: the default is the PRODUCTION setting (decoration on), and
// the returned function restores exactly the value that was replaced — including
// when calls nest, which is what lets a differential test set one arm inside a
// subtest without leaking it to the next.
//
// It is NOT t.Parallel: it writes the process-global control.
func TestSetKeyDecorationDisabledRestores(t *testing.T) {
	if KeyDecorationDisabled() {
		t.Fatal("default must be false (decoration ON): production must never take the legacy path")
	}

	restoreOuter := SetKeyDecorationDisabled(true)
	if !KeyDecorationDisabled() {
		t.Fatal("after SetKeyDecorationDisabled(true) the control did not read back as set")
	}

	// Nested: setting the same value, then restoring, must leave the OUTER
	// value in place rather than the package default.
	restoreInner := SetKeyDecorationDisabled(false)
	if KeyDecorationDisabled() {
		t.Fatal("nested SetKeyDecorationDisabled(false) did not take effect")
	}
	restoreInner()
	if !KeyDecorationDisabled() {
		t.Fatal("inner restore reset to the package default instead of the outer value")
	}

	restoreOuter()
	if KeyDecorationDisabled() {
		t.Fatal("outer restore did not return the control to its default")
	}
}
