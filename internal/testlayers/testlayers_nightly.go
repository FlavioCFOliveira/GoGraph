//go:build nightly

package testlayers

// IsNightly reports, at compile time, whether the binary was built
// with the `nightly` build tag. The variant of this constant
// defined here is true; the inverse (`!nightly`) variant in
// testlayers_nightly_stub.go sets it to false.
const IsNightly = true
