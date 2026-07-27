package lpg

// export_test.go — package-internal helpers exposed to the external lpg_test
// package. Being in a _test.go file, none of this ships in the library.

// DecodeSlotLabelForTest exposes [decodeSlotLabel] to the external test package.
//
// It exists so a test oracle can read the adjacency's stored slot label the way
// the production code reads it, instead of re-deriving the encoding. That matters
// because the encoding is exactly where the degree primitive was first wrong: the
// adjacency column holds encodeSlotLabel(id) — id+1, reserving 0 for "no label" —
// and comparing a raw LabelID against it silently counts a different
// relationship type. A test that hard-coded "+1" would keep passing if the
// encoding ever changed; one that calls the real codec would not.
func DecodeSlotLabelForTest(v uint32) (LabelID, bool) { return decodeSlotLabel(v) }
