package main

import "os"

// Example pins the deterministic stdout of the social-network analytics
// example. Go's test framework captures everything run writes to
// os.Stdout and compares it against the // Output: block below, so a
// future change that alters the report — PageRank order, community
// membership, or friend-of-friend recommendations — is caught as a
// regression.
func Example() {
	_ = run(os.Stdout)
	// Output:
	// Influence (PageRank):
	//   carol    0.1855
	//   alice    0.1786
	//   erin     0.1786
	//   dave     0.1294
	//   bob      0.1270
	//   grace    0.1270
	//   frank    0.0740
	//
	// Communities (Leiden):
	//   community 0: [alice bob dave]
	//   community 1: [erin grace]
	//   community 2: [carol frank]
	//
	// Friend-of-friend recommendations for alice:
	//   -> dave
	//   -> frank
	//   -> grace
}
