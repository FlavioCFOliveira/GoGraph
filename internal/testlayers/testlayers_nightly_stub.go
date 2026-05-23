//go:build !nightly

package testlayers

// IsNightly reports, at compile time, whether the binary was built
// with the `nightly` build tag. This stub is selected when the tag
// is absent; the variant in testlayers_nightly.go sets it to true.
const IsNightly = false
