//go:build soak || nightly

package testlayers

// IsSoak reports, at compile time, whether the binary was built
// with the `soak` or `nightly` build tag. The variant of this
// constant defined here is true; the inverse (`!soak && !nightly`)
// variant in testlayers_soak_stub.go sets it to false.
// Nightly implies soak: any binary built with -tags=nightly also
// activates the soak layer, consistent with the layer hierarchy
// documented in docs/test-layers.md.
const IsSoak = true
