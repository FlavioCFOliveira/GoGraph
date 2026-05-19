package search

import "gograph/graph"

// uint64ToNodeID is a tiny helper used by APSP tests to avoid
// importing graph in every test file.
func uint64ToNodeID(i int) graph.NodeID { return graph.NodeID(uint64(i)) }
