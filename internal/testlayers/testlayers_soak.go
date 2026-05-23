//go:build soak

package testlayers

// IsSoak reports, at compile time, whether the binary was built
// with the `soak` build tag. The variant of this constant defined
// here is true; the inverse (`!soak`) variant in
// testlayers_soak_stub.go sets it to false.
const IsSoak = true
