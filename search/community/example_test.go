package community_test

import (
	"fmt"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search/community"
)

// buildTwoTriangles returns an undirected CSR of two triangles
// {0,1,2} and {3,4,5} linked by a single bridge edge 2-3, together
// with its mapper. It is the canonical fixture for showing a
// community detector separating two densely-knit groups.
func buildTwoTriangles() (*csr.CSR[struct{}], *adjlist.AdjList[int, struct{}]) {
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for _, e := range [][2]int{{0, 1}, {1, 2}, {0, 2}, {3, 4}, {4, 5}, {3, 5}} {
		_ = a.AddEdge(e[0], e[1], struct{}{})
	}
	_ = a.AddEdge(2, 3, struct{}{}) // the single inter-group bridge
	return csr.BuildFromAdjList(a), a
}

// sameCommunity reports whether every listed user value shares one
// community label in the partition, resolving each through the mapper.
func sameCommunity(p community.Partition, m *adjlist.AdjList[int, struct{}], values ...int) bool {
	first, _ := m.Mapper().Lookup(values[0])
	want := p.Community[first]
	for _, v := range values[1:] {
		id, _ := m.Mapper().Lookup(v)
		if p.Community[id] != want {
			return false
		}
	}
	return true
}

// ExampleLeiden detects communities with the Leiden algorithm. The
// concrete community IDs are an implementation detail, so the example
// asserts stable derived facts: the number of communities found and
// that each triangle stays together while the two triangles separate.
func ExampleLeiden() {
	c, m := buildTwoTriangles()

	p := community.Leiden(c, community.DefaultLeidenOptions())

	fmt.Printf("communities: %d\n", p.NumCommunities)
	fmt.Printf("triangle {0,1,2} together: %v\n", sameCommunity(p, m, 0, 1, 2))
	fmt.Printf("triangle {3,4,5} together: %v\n", sameCommunity(p, m, 3, 4, 5))
	fmt.Printf("the two triangles merged: %v\n", sameCommunity(p, m, 0, 3))
	// Output:
	// communities: 2
	// triangle {0,1,2} together: true
	// triangle {3,4,5} together: true
	// the two triangles merged: false
}

// ExampleLabelPropagation detects communities with the near-linear
// label-propagation algorithm. As with Leiden, the raw labels are not
// asserted; the example checks the community count and that each dense
// triangle is recovered as a single group.
func ExampleLabelPropagation() {
	c, m := buildTwoTriangles()

	p := community.LabelPropagation(c, community.DefaultLabelPropagationOptions())

	fmt.Printf("communities: %d\n", p.NumCommunities)
	fmt.Printf("triangle {0,1,2} together: %v\n", sameCommunity(p, m, 0, 1, 2))
	fmt.Printf("triangle {3,4,5} together: %v\n", sameCommunity(p, m, 3, 4, 5))
	// Output:
	// communities: 2
	// triangle {0,1,2} together: true
	// triangle {3,4,5} together: true
}
