package txn

import "fmt"

// goFormat renders v as its default Go string form ("%v"). Centralised
// so we can swap to a typed codec later without touching call sites.
func goFormat[N comparable](v N) string { return fmt.Sprintf("%v", v) }
