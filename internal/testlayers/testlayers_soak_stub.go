//go:build !soak

package testlayers

// IsSoak reports, at compile time, whether the binary was built
// with the `soak` build tag. This stub is selected when the tag is
// absent; the variant in testlayers_soak.go sets it to true.
const IsSoak = false
