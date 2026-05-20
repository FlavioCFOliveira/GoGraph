// Package procs defines the procedure registry for the Cypher executor.
//
// A [Registry] stores [ProcEntry] values keyed by the fully-qualified procedure
// name ("namespace.name") and is consulted at plan-build time by
// [exec.ProcedureCallOp]. Built-in procedures (db.*) are registered via
// [RegisterBuiltins]; custom procedures may be added via [Register].
//
// # Circular-import avoidance
//
// This package must NOT import gograph/cypher/exec. exec imports procs.
//
// # Concurrency
//
// Registry is safe for concurrent use.
package procs

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"gograph/cypher/expr"
)

// ─────────────────────────────────────────────────────────────────────────────
// Types
// ─────────────────────────────────────────────────────────────────────────────

// NamedType pairs an output column name with its expected value kind.
type NamedType struct {
	// Name is the column name exposed via YIELD.
	Name string
	// Kind is the expected expr.Kind of values in this column.
	Kind expr.Kind
}

// Signature describes the calling contract of a procedure.
type Signature struct {
	// Namespace is the optional namespace path (e.g. ["db"] for db.labels()).
	Namespace []string
	// Name is the bare procedure name.
	Name string
	// Inputs lists the expected kinds for each positional argument.
	Inputs []expr.Kind
	// Outputs lists the named output columns produced by the procedure.
	Outputs []NamedType
}

// ProcImpl is the runtime function for a procedure. It receives the
// context and the evaluated argument values and returns all result rows.
// Each row must have exactly len(Signature.Outputs) columns.
type ProcImpl func(ctx context.Context, args []expr.Value) ([][]expr.Value, error)

// ProcEntry binds a Signature to its implementation.
type ProcEntry struct {
	Sig  Signature
	Impl ProcImpl
}

// ─────────────────────────────────────────────────────────────────────────────
// Sentinel errors
// ─────────────────────────────────────────────────────────────────────────────

// ErrProcAlreadyExists is returned by [Registry.Register] when a procedure
// with the same fully-qualified name is already registered.
var ErrProcAlreadyExists = errors.New("procs: procedure already exists")

// ErrProcNotFound is returned by [Registry.Lookup] when no procedure matching
// the given namespace and name is registered.
var ErrProcNotFound = errors.New("procs: procedure not found")

// ─────────────────────────────────────────────────────────────────────────────
// Registry
// ─────────────────────────────────────────────────────────────────────────────

// Registry is a thread-safe store of registered procedures keyed by their
// fully-qualified name ("ns1.ns2.name").
//
// Registry is safe for concurrent use.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]ProcEntry
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		entries: make(map[string]ProcEntry),
	}
}

// fqn returns the fully-qualified name for (namespace, name).
func fqn(namespace []string, name string) string {
	if len(namespace) == 0 {
		return name
	}
	return strings.Join(namespace, ".") + "." + name
}

// Register adds a procedure to the registry. It returns a wrapped
// [ErrProcAlreadyExists] when a procedure with the same fully-qualified name
// is already registered.
//
//nolint:gocritic // hugeParam: Signature is passed by value intentionally; callers own the struct
func (r *Registry) Register(sig Signature, impl ProcImpl) error {
	key := fqn(sig.Namespace, sig.Name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.entries[key]; exists {
		return fmt.Errorf("%w: %s", ErrProcAlreadyExists, key)
	}
	r.entries[key] = ProcEntry{Sig: sig, Impl: impl}
	return nil
}

// Lookup retrieves the ProcEntry for (namespace, name). It returns a wrapped
// [ErrProcNotFound] when no such procedure is registered.
func (r *Registry) Lookup(namespace []string, name string) (ProcEntry, error) {
	key := fqn(namespace, name)
	r.mu.RLock()
	entry, ok := r.entries[key]
	r.mu.RUnlock()
	if !ok {
		return ProcEntry{}, fmt.Errorf("%w: %s", ErrProcNotFound, key)
	}
	return entry, nil
}

// List returns all registered signatures in lexicographic order of their
// fully-qualified names.
func (r *Registry) List() []Signature {
	r.mu.RLock()
	sigs := make([]Signature, 0, len(r.entries))
	for _, e := range r.entries {
		sigs = append(sigs, e.Sig)
	}
	r.mu.RUnlock()
	sort.Slice(sigs, func(i, j int) bool {
		return fqn(sigs[i].Namespace, sigs[i].Name) < fqn(sigs[j].Namespace, sigs[j].Name)
	})
	return sigs
}
