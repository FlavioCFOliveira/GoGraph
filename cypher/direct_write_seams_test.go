package cypher_test

import "github.com/FlavioCFOliveira/GoGraph/graph/lpg"

// zeroViewRevive revives n through a WriteView over the zero WriteTx and returns
// the direct write's refusal, if any (rmp #2947).
func zeroViewRevive(g *lpg.Graph[string, float64], n string) error {
	return g.Writer(lpg.WriteTx{}).Revive(n)
}
