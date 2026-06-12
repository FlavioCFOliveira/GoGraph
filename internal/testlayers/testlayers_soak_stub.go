//go:build !soak && !nightly

package testlayers

// IsSoak reports, at compile time, whether the binary was built
// with the `soak` build tag. This stub is selected when neither
// `soak` nor `nightly` is active; the variant in testlayers_soak.go
// sets it to true for both of those tags.
const IsSoak = false
