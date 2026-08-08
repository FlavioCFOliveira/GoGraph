package mvcc

// stores.go — the fixed set of versioned stores a conflict can be detected in
// (rmp #2312).
//
// # Why the set is closed
//
// The conflict series is published per store, because knowing that a workload
// contends without knowing WHICH structure contended is not actionable. A
// per-store series needs bounded cardinality, and the only way to bound it is to
// enumerate the stores rather than to accept whatever string a call site passes.
//
// So the store name is drawn from the constants below and from nowhere else, and
// [ConflictStoreIndex] maps it to a dense index the counter bank can address
// without a map lookup or an allocation. An unrecognised name is counted under
// [StoreOther] rather than dropped or trusted: a new store that forgets to add
// its constant loses its attribution, never the count, and
// TestConflictStores_EveryNameHasAnIndex fails on the omission.
//
// # Two spellings, on purpose
//
// The constant is the HUMAN name, which is what [Conflict]'s message reads and
// what an operator sees in an error. [ConflictStoreMetric] is the METRIC suffix,
// which carries no space, so the published series name is decided here rather
// than by whatever sanitising a backend happens to apply. Prometheus rejects a
// space in a metric name, and a name that only becomes valid downstream is a name
// nobody can predict from the source.

// The versioned stores a write-write conflict can be detected in. The value is
// the store's human name, as it appears in a [Conflict]'s message.
const (
	StoreNodeLabels      = "node labels"
	StoreNodeProperties  = "node properties"
	StoreNodeExistence   = "node existence"
	StoreAdjacency       = "adjacency"
	StoreEdgeTypes       = "edge relationship types"
	StoreEdgeTypesHandle = "edge relationship types by handle"
	StoreEdgeTypesOrd    = "edge relationship types by ordinal"
	StoreEdgePropsHandle = "edge properties by handle"
	StoreEdgePropsOrd    = "edge properties by ordinal"
	// StoreNodeConstraint is the per-node constraint stamp (rmp #2353). It is the
	// one store here whose granularity is the NODE rather than one of the node's
	// substores, and it exists precisely because the others are narrower: a
	// declared invariant spanning two substores cannot be enforced by conflict
	// detection that never compares them. It is stamped only for nodes under an
	// active existence constraint, so a schema declaring none never reaches it.
	StoreNodeConstraint = "node constraint"
	// StoreOther is where a name that is not one of the above is counted. It is
	// not a store; it is the bucket that keeps the cardinality bounded without
	// losing the count.
	StoreOther = "other"
)

// conflictStore pairs a store's human name with its metric suffix.
type conflictStore struct {
	name   string
	metric string
}

// conflictStores is the dense, ordered table [ConflictStoreIndex] indexes into.
// StoreOther is last so a new store can be appended before it without moving it.
var conflictStores = [...]conflictStore{
	{StoreNodeLabels, "node_labels"},
	{StoreNodeProperties, "node_properties"},
	{StoreNodeExistence, "node_existence"},
	{StoreAdjacency, "adjacency"},
	{StoreEdgeTypes, "edge_types"},
	{StoreEdgeTypesHandle, "edge_types_by_handle"},
	{StoreEdgeTypesOrd, "edge_types_by_ordinal"},
	{StoreEdgePropsHandle, "edge_props_by_handle"},
	{StoreEdgePropsOrd, "edge_props_by_ordinal"},
	{StoreNodeConstraint, "node_constraint"},
	{StoreOther, "other"},
}

// ConflictStoreCount is how many per-store conflict buckets exist, including
// [StoreOther].
const ConflictStoreCount = len(conflictStores)

// ConflictStoreIndex returns the dense index of store, or the index of
// [StoreOther] when the name is not one of the constants above.
//
// A linear scan over ten entries, deliberately: it runs once per conflicting
// transaction — a path that is by definition exceptional — and a switch or a map
// would either duplicate the table or allocate a hash lookup to save nanoseconds
// nobody is waiting on.
func ConflictStoreIndex(store string) int {
	for i := range conflictStores {
		if conflictStores[i].name == store {
			return i
		}
	}
	return ConflictStoreCount - 1
}

// ConflictStoreName returns the human name of the bucket at index i.
func ConflictStoreName(i int) string { return conflictStores[i].name }

// ConflictStoreMetric returns the metric-name suffix of the bucket at index i:
// the human name with no character a Prometheus metric name may not carry.
func ConflictStoreMetric(i int) string { return conflictStores[i].metric }
