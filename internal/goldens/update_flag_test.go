package goldens_test

import (
	"flag"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/goldens"
)

// TestUpdateRequested_FlagTrue verifies that UpdateRequested returns true
// after the -update flag is set to true via flag.Set (simulating flag.Parse
// with -update=true). This is the gate test for #1392: the pre-fix code read
// the flag value at package-init time (always false before flag.Parse) so
// UpdateRequested always returned false regardless of the -update flag.
func TestUpdateRequested_FlagTrue(t *testing.T) {
	f := flag.Lookup("update")
	if f == nil {
		t.Fatal("-update flag not registered")
	}

	// Capture original value and restore after the test.
	orig := f.Value.String()
	t.Cleanup(func() { _ = flag.Set("update", orig) })

	if err := flag.Set("update", "true"); err != nil {
		t.Fatalf("flag.Set update=true: %v", err)
	}

	if !goldens.UpdateRequested() {
		t.Error("UpdateRequested() = false after -update=true; want true")
	}
}

// TestUpdateRequested_FlagFalse verifies that UpdateRequested returns false
// when -update is false and the env variable is unset.
func TestUpdateRequested_FlagFalse(t *testing.T) {
	f := flag.Lookup("update")
	if f == nil {
		t.Fatal("-update flag not registered")
	}

	orig := f.Value.String()
	t.Cleanup(func() { _ = flag.Set("update", orig) })

	if err := flag.Set("update", "false"); err != nil {
		t.Fatalf("flag.Set update=false: %v", err)
	}

	t.Setenv("GOGRAPH_UPDATE_GOLDENS", "0")

	if goldens.UpdateRequested() {
		t.Error("UpdateRequested() = true with -update=false and env unset; want false")
	}
}

// TestUpdateRequested_NoPanic verifies that UpdateRequested does not panic
// even when -update flag's Value lacks a Get() method (simulated by checking
// the comma-ok guard is present). We verify indirectly: the standard
// flag.Bool Value does implement Get(), so we just confirm no panic occurs.
func TestUpdateRequested_NoPanic(t *testing.T) {
	// Should not panic regardless of flag state.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("UpdateRequested panicked: %v", r)
		}
	}()
	_ = goldens.UpdateRequested()
}
