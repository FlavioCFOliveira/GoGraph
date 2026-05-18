// Package schema declares the optional type schema for a labelled
// property graph: which labels exist, which property keys exist, and
// which [PropertyKind] each property carries.
//
// The schema is advisory by default — callers may opt into runtime
// validation via [Schema.Validate] before applying a write — but it
// is also the surface a future persistence layer (Sprint 3) will
// serialise alongside snapshots so opens can reject incompatible
// data.
package schema

import (
	"errors"
	"fmt"
	"sync"

	"gograph/graph/lpg"
)

// ErrTypeMismatch is returned by [Schema.Validate] when a value's
// kind does not match the kind declared for its property key.
var ErrTypeMismatch = errors.New("schema: property type mismatch")

// ErrUnknownProperty is returned by [Schema.Validate] when a value
// is supplied for a property key that has not been registered.
var ErrUnknownProperty = errors.New("schema: unknown property key")

// Schema is the registry of declared labels and property kinds. It
// is safe for concurrent use.
type Schema struct {
	mu         sync.RWMutex
	labelReg   *lpg.LabelRegistry
	propReg    *lpg.PropertyKeyRegistry
	labels     map[string]lpg.LabelID
	properties map[string]propertyDecl
}

type propertyDecl struct {
	id   lpg.PropertyKeyID
	kind lpg.PropertyKind
}

// New returns an empty schema. The supplied label and property-key
// registries are reused — typically those owned by the [lpg.Graph]
// the schema describes — so identifiers stay consistent across the
// schema and the live graph.
func New(labels *lpg.LabelRegistry, properties *lpg.PropertyKeyRegistry) *Schema {
	if labels == nil {
		labels = lpg.NewLabelRegistry()
	}
	if properties == nil {
		properties = lpg.NewPropertyKeyRegistry()
	}
	return &Schema{
		labelReg:   labels,
		propReg:    properties,
		labels:     make(map[string]lpg.LabelID),
		properties: make(map[string]propertyDecl),
	}
}

// RegisterLabel records name as a declared label and returns its
// stable [lpg.LabelID]. Idempotent.
func (s *Schema) RegisterLabel(name string) lpg.LabelID {
	s.mu.RLock()
	if id, ok := s.labels[name]; ok {
		s.mu.RUnlock()
		return id
	}
	s.mu.RUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.labels[name]; ok {
		return id
	}
	id := s.labelReg.Intern(name)
	s.labels[name] = id
	return id
}

// RegisterProperty records name as a property key carrying the given
// kind, returning the stable [lpg.PropertyKeyID]. Registering the same
// name twice with different kinds is rejected with an error.
func (s *Schema) RegisterProperty(name string, kind lpg.PropertyKind) (lpg.PropertyKeyID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.properties[name]; ok {
		if d.kind != kind {
			return 0, fmt.Errorf("%w: %q is %d, attempted %d", ErrTypeMismatch, name, d.kind, kind)
		}
		return d.id, nil
	}
	id := s.propReg.Intern(name)
	s.properties[name] = propertyDecl{id: id, kind: kind}
	return id, nil
}

// PropertyKind returns the kind declared for name and true, or 0 and
// false when name has not been registered.
func (s *Schema) PropertyKind(name string) (lpg.PropertyKind, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.properties[name]
	if !ok {
		return 0, false
	}
	return d.kind, true
}

// Validate checks value against the declared kind for the named
// property. Returns nil on a match, [ErrUnknownProperty] when the
// property is unregistered, or [ErrTypeMismatch] when the value's
// kind disagrees with the declaration.
func (s *Schema) Validate(propertyName string, value lpg.PropertyValue) error {
	s.mu.RLock()
	d, ok := s.properties[propertyName]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownProperty, propertyName)
	}
	if value.Kind() != d.kind {
		return fmt.Errorf("%w: %q declared %d, value is %d",
			ErrTypeMismatch, propertyName, d.kind, value.Kind())
	}
	return nil
}

// Labels returns a snapshot of the declared label names.
func (s *Schema) Labels() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.labels))
	for name := range s.labels {
		out = append(out, name)
	}
	return out
}

// Properties returns a snapshot of declared (name, kind) pairs.
func (s *Schema) Properties() map[string]lpg.PropertyKind {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]lpg.PropertyKind, len(s.properties))
	for name, d := range s.properties {
		out[name] = d.kind
	}
	return out
}

// LabelRegistry returns the underlying label registry. The schema and
// the registry share state; mutations on the registry leak out of the
// schema's validation surface but the IDs stay consistent.
func (s *Schema) LabelRegistry() *lpg.LabelRegistry { return s.labelReg }

// PropertyKeyRegistry returns the underlying property-key registry.
func (s *Schema) PropertyKeyRegistry() *lpg.PropertyKeyRegistry { return s.propReg }
