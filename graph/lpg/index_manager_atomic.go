package lpg

import (
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// atomicIndexManager provides lock-free, sequentially-consistent get/set for
// an optional [index.Manager] pointer. It mirrors the [atomicValidator]
// pattern already used for schema enforcement.
//
// Because [index.Manager] is a concrete struct (not an interface), no holder
// wrapper is needed: [atomic.Pointer] operates directly on *index.Manager.
type atomicIndexManager struct {
	p atomic.Pointer[index.Manager]
}

// load returns the current manager, or nil when none has been installed.
func (a *atomicIndexManager) load() *index.Manager {
	return a.p.Load()
}

// store installs m as the current manager. Passing nil detaches the
// previous manager. The store is sequentially consistent: any goroutine
// that calls [atomicIndexManager.load] after this store returns will
// observe m (or a later value).
func (a *atomicIndexManager) store(m *index.Manager) {
	a.p.Store(m)
}
