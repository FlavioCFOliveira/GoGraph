//go:build soak || nightly || soakfull || stress

package testlayers

// IsSoak reports, at compile time, whether the binary was built with a build
// tag in the soak family. The variant defined here is true; the inverse
// variant in testlayers_soak_stub.go sets it to false.
//
// The soak family is `soak`, `nightly`, `soakfull`, and `stress`: nightly
// implies soak (the layer hierarchy), and the pre-existing `soakfull`/`stress`
// tags are documented as part of the soak family (docs/test-layers.md). Honour
// them here so a test gated by both a `//go:build soakfull` header and
// RequireSoak does not skip under its own `-tags=soakfull` invocation (#1810).
const IsSoak = true
