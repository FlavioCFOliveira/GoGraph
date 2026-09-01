package cypher

// deprecated.go — exported symbols kept solely so code written against an
// earlier release keeps compiling.
//
// Nothing in this file is read by the engine. Each symbol carries a
// `Deprecated:` clause as the first line of its doc comment, which is what
// staticcheck's SA1019 and every editor's hover surface consume, and each states
// plainly that it no longer has any effect rather than describing behaviour it
// no longer has.

// DefaultEdgeTypeFilterCacheCapacity is the default the retired edge-type-filter
// cache used to take.
//
// Deprecated: it configures nothing. The per-relationship-type-set filter cache
// this constant sized was retired by rmp #2251: relationship-type information is
// now a slot-aligned column stored beside the CSR pair it describes, and the
// column is INDEPENDENT of which types a query names, so there is no longer a
// per-type-set population to bound. There is no replacement constant, because
// there is no longer anything of this kind to size.
//
// The value is unchanged so that code comparing against it still compiles and
// still reads the same number.
const DefaultEdgeTypeFilterCacheCapacity = 256
