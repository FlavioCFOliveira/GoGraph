package synclatency

import "testing"

// TestForTest_Switches pins the two environment controls: "off" disables the
// latency, and a fixed seed is the seed the latency draws from.
func TestForTest_Switches(t *testing.T) {
	t.Setenv(EnvSwitch, "off")
	if l := ForTest(t); l != nil {
		t.Fatalf("ForTest with %s=off = %v, want nil", EnvSwitch, l)
	}
	t.Setenv(EnvSwitch, "")
	t.Setenv(EnvSeed, "12345")
	l := ForTest(t)
	if l == nil || l.Seed() != 12345 {
		t.Fatalf("ForTest with %s=12345: %v", EnvSeed, l)
	}
	if lo, hi := l.Bounds(); lo != Min || hi != Max {
		t.Fatalf("bounds %v-%v, want %v-%v", lo, hi, Min, Max)
	}
	t.Setenv(EnvSeed, "not-a-number")
	if _, err := Seed(); err == nil {
		t.Fatal("Seed accepted a malformed seed")
	}
}
