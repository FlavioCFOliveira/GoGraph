package cypher

import (
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// reltype_column.go — building the slot-aligned relationship-type column and
// serving it beside the CSR pair it describes (rmp #2251).
//
// # What this replaced
//
// A typed expand used to test a slot's type by probing a map[uint64]string keyed
// by absolute forward CSR position. That map held one entry per ACCEPTED slot
// across the whole graph — Θ(E) on a graph with one dominant relationship type —
// and it was probed once per CSR slot touched, in a hot loop, with the reverse
// direction paying an O(log d + r) position recovery first. Because the map's
// contents depended on which types the query accepted, it also had to be built
// once per (type set, graph state) and amortised through its own bounded LRU with
// its own mutex.
//
// The column is the same information laid out slot-for-slot beside the arcs, and
// it is TYPE-SET INDEPENDENT: it records what each arc IS, not whether some
// particular pattern would accept it. So it is built once per CSR pair, cached
// beside that pair, and reused by every typed pattern over that graph state
// whatever types they name. The per-type-set LRU had nothing left to amortise and
// was retired with it.
//
// # Why it lives here and not in graph/csr
//
// The column is filled against FINAL arc positions — after [csr.OrderRuns] has
// permuted each source's neighbour run — and it is derived from LPG state the csr
// package does not have. Putting it on csr.CSR would also drag it into csr.bin,
// snapshot capture and the cross-process byte-equality gates for no benefit: the
// planner is the only consumer, and it holds both halves already.
//
// # The resolution is NOT re-implemented here
//
// Every code in the column comes from [forEachResolvedSlotType], which IS the
// pre-#2251 filter builder's three-tier resolver, verbatim. Read its doc before
// touching anything in this file: the adjacency label column alone is not
// authoritative, and a column filled from it would silently reintroduce four
// closed defects.

// relTypeCodesFor encodes the pattern's accepted relationship-type names into the
// column's code space, dropping any name the graph has never interned.
//
// Dropping is correct rather than lossy: a code exists only for a type some
// relationship actually carries, so a name with no LabelID cannot be the type of
// any arc and could never have appeared in the filter map either. The result may
// therefore be empty, which admits nothing — exactly what `MATCH ()-[:NEVER_USED]->()`
// must return.
func relTypeCodesFor(g *lpg.ReadView[string, float64], relTypes []string) []uint32 {
	if len(relTypes) == 0 {
		return nil
	}
	reg := g.Registry()
	if reg == nil {
		return nil
	}
	codes := make([]uint32, 0, len(relTypes))
	for _, name := range relTypes {
		if lid, ok := reg.Lookup(name); ok {
			codes = append(codes, lpg.EncodeSlotLabel(lid))
		}
	}
	return codes
}

// buildRelTypeColumn resolves every arc of fwd and returns the slot-aligned type
// column for the pair (fwd, rev).
//
// The dense array holds the FIRST type each arc carries; the sparse exception map
// holds the second and later types of the rare arc carrying more than one. That
// map is nil for every graph Cypher built, because an openCypher relationship
// carries exactly one type — it exists for the Go API, whose per-handle label bag
// is a set and whose pair overflow list holds further types.
//
// The reverse half of the column is derived by [exec.NewRelTypeColumnFor].
func buildRelTypeColumn(
	g *lpg.ReadView[string, float64], fwd, rev *csr.CSR[float64],
) *exec.RelTypeColumn {
	metrics.IncCounter("cypher.reltype_column.builds", 1)
	reg := g.Registry()
	fwdCodes := make([]uint32, len(fwd.EdgesSlice()))
	var fwdExtra map[uint64][]uint32
	forEachResolvedSlotType(g, fwd, func(pos uint64, types []string) {
		if pos >= uint64(len(fwdCodes)) {
			return
		}
		for _, name := range types {
			lid, ok := reg.Lookup(name)
			if !ok {
				// A name the resolver produced always came from the registry, so this
				// is unreachable in practice. Skipping is nonetheless the only safe
				// answer: an unencodable name cannot be matched by any pattern's code
				// set either, so it can admit nothing whichever way it is handled.
				continue
			}
			code := lpg.EncodeSlotLabel(lid)
			if fwdCodes[pos] == 0 {
				fwdCodes[pos] = code
				continue
			}
			if fwdCodes[pos] == code {
				continue
			}
			if fwdExtra == nil {
				fwdExtra = make(map[uint64][]uint32)
			}
			if !containsCode(fwdExtra[pos], code) {
				fwdExtra[pos] = append(fwdExtra[pos], code)
			}
		}
	})
	// rev may be nil for a caller that holds only a forward CSR; the constructor
	// reads that as "no reverse pairing", not as an error.
	var revAdj exec.CSRAdjacency
	if rev != nil {
		revAdj = rev
	}
	col := exec.NewRelTypeColumnFor(fwd, revAdj, fwdCodes, fwdExtra)
	// The column is a structure a warm Engine RETAINS for the lifetime of a graph
	// state, so its footprint is operational information, not trivia: it is
	// 4 bytes per arc per direction plus whatever the multi-type patch list costs,
	// and a deployment weighing [EngineOptions.DisableCSRPairCache] needs the
	// number rather than the formula.
	metrics.SetGauge("cypher.reltype_column.bytes", float64(col.RelTypeColumnBytes()))
	return col
}

// containsCode reports whether codes already holds code. The patch list of a
// multi-type arc holds a handful of entries at most, so a linear scan is both
// faster and smaller than a set.
func containsCode(codes []uint32, code uint32) bool {
	for _, c := range codes {
		if c == code {
			return true
		}
	}
	return false
}
