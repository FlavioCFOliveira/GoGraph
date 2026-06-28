//go:build !soak && !nightly && !soakfull && !stress

package testlayers

// IsSoak reports, at compile time, whether the binary was built with a build
// tag in the soak family. This stub is selected when none of `soak`, `nightly`,
// `soakfull`, or `stress` is active; the variant in testlayers_soak.go sets it
// to true for any of those tags (#1810).
const IsSoak = false
