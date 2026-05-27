package sema

import "gograph/cypher/ast"

// Symbol records the introduction point of a variable within a scope.
type Symbol struct {
	// Name is the variable name exactly as written in the query.
	Name string
	// Pos is the source position where the variable was first introduced.
	Pos ast.Position
	// Type is a coarse type hint populated by later analysis passes (e.g.
	// "node", "relationship", "path", "any"). The scope pass uses the empty
	// string as a catch-all; callers must not depend on this value being set.
	Type string
}

// Scope is a single layer of the variable-scope stack.
// Scopes form a parent chain: child scopes inherit visibility of all symbols
// defined in their ancestors unless a WITH boundary resets the chain.
//
// Scope is not safe for concurrent use; callers must synchronise externally.
type Scope struct {
	parent  *Scope
	symbols map[string]*Symbol
}

// NewScope allocates a fresh root scope with no parent.
func NewScope() *Scope {
	return &Scope{symbols: make(map[string]*Symbol)}
}

// newScope is the package-internal alias for NewScope.
func newScope() *Scope { return NewScope() }

// Child creates a child scope that inherits the current scope's visibility.
// The child shares read access to the parent chain but has its own symbol table,
// so definitions in the child do not pollute the parent.
func (s *Scope) Child() *Scope {
	return &Scope{
		parent:  s,
		symbols: make(map[string]*Symbol),
	}
}

// Define introduces a new symbol in this scope.
// It returns a KindRedeclaration error if the name is already defined in this
// exact scope (shadowing a parent-scope symbol is permitted and does not error).
func (s *Scope) Define(name string, pos ast.Position, typ string) *ScopeError {
	if _, exists := s.symbols[name]; exists {
		return redeclarationError(name, pos)
	}
	s.symbols[name] = &Symbol{Name: name, Pos: pos, Type: typ}
	return nil
}

// Lookup searches for name starting in the current scope and walking up the
// parent chain. It returns the Symbol and true when found, nil and false
// otherwise.
func (s *Scope) Lookup(name string) (*Symbol, bool) {
	for cur := s; cur != nil; cur = cur.parent {
		if sym, ok := cur.symbols[name]; ok {
			return sym, true
		}
	}
	return nil, false
}

// LookupLocal checks only this scope (no parent walk).
func (s *Scope) LookupLocal(name string) (*Symbol, bool) {
	sym, ok := s.symbols[name]
	return sym, ok
}

// Names returns all symbol names defined in this scope only (no parent walk).
// The returned slice has no guaranteed order.
func (s *Scope) Names() []string {
	out := make([]string, 0, len(s.symbols))
	for k := range s.symbols {
		out = append(out, k)
	}
	return out
}

// reset replaces this scope's symbol table with a new empty map.
// Used internally to implement the WITH boundary: the caller rebuilds the scope
// with only the projected names.
func (s *Scope) reset() {
	s.symbols = make(map[string]*Symbol)
	s.parent = nil
}

// scopeHasAnyName reports whether the scope (or any parent in its chain)
// contains at least one defined symbol. Used by [projectionCheck] to
// detect star projections issued with truly nothing in scope.
func scopeHasAnyName(s *Scope) bool {
	for cur := s; cur != nil; cur = cur.parent {
		if len(cur.symbols) > 0 {
			return true
		}
	}
	return false
}
