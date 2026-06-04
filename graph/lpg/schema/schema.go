// Package schema declares the optional type schema for a labelled
// property graph: which labels exist, which property keys exist, which
// [lpg.PropertyKind] each property carries, and which properties each
// label requires.
//
// A *Schema is a runtime enforcement hook, not merely advisory. Install
// one on a graph with [github.com/FlavioCFOliveira/GoGraph/graph/lpg.Graph.SetValidator]
// and the two declared invariants are enforced on the write path:
//
//   - Per-property typing — [Schema.Validate] runs inside every
//     SetNodeProperty / SetEdgeProperty call, rejecting a value whose kind
//     disagrees with its declaration before the write is applied.
//   - Required-property existence — [Schema.ValidateNode] runs when a node
//     is finalised, via lpg.Graph.ValidateNode, rejecting a node that is
//     missing a property its label requires (see [Schema.RequireProperty]).
//
// The split is deliberate: typing is decided from a single value at the
// mutation point, but existence can only be decided once a node is complete
// (a node acquires its label before the property that label requires), so
// existence is enforced at the finalisation boundary rather than mid-build.
//
// Callers may also run [Schema.Validate] and [Schema.ValidateNode] directly,
// before applying a write, to reject incompatible data early. The schema is
// also the surface the persistence layer serialises alongside snapshots so
// opens can reject incompatible data.
package schema

import (
	"errors"
	"fmt"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ErrTypeMismatch is returned by [Schema.Validate] when a value's
// kind does not match the kind declared for its property key.
var ErrTypeMismatch = errors.New("schema: property type mismatch")

// ErrUnknownProperty is returned by [Schema.Validate] when a value
// is supplied for a property key that has not been registered.
var ErrUnknownProperty = errors.New("schema: unknown property key")

// ErrMissingRequired is returned by [Schema.ValidateNode] when a node
// is missing a property that its label requires.
var ErrMissingRequired = errors.New("schema: missing required property")

// Schema is the registry of declared labels and property kinds. It
// is safe for concurrent use.
type Schema struct {
	mu         sync.RWMutex
	labelReg   *lpg.LabelRegistry
	propReg    *lpg.PropertyKeyRegistry
	labels     map[string]lpg.LabelID
	properties map[string]propertyDecl
	// required maps label name → set of property names that must be present.
	required map[string]map[string]struct{}
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
		required:   make(map[string]map[string]struct{}),
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

// RequireProperty records that any node carrying labelName must have
// propertyName set. The call is idempotent: repeating the same
// (label, property) pair has no effect. The label and property do not
// need to be pre-registered — the requirement is stored regardless and
// evaluated by [Schema.ValidateNode].
//
// When the schema is installed on a graph via
// [github.com/FlavioCFOliveira/GoGraph/graph/lpg.Graph.SetValidator], the
// requirement is enforced — not merely advisory — at the node-finalisation
// boundary through lpg.Graph.ValidateNode: a finalised node missing the
// required property is rejected with [ErrMissingRequired].
func (s *Schema) RequireProperty(labelName, propertyName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.required[labelName]
	if !ok {
		m = make(map[string]struct{})
		s.required[labelName] = m
	}
	m[propertyName] = struct{}{}
}

// ValidateNode checks that a node described by labels and props
// satisfies every requirement recorded in the schema:
//
//  1. Every property name declared as required for any of the node's
//     labels is present in props.
//  2. Every property present in props whose name has been registered
//     carries a value of the declared kind.
//
// The first violation encountered is returned. On success, nil is returned.
func (s *Schema) ValidateNode(labels []string, props map[string]lpg.PropertyValue) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Check required properties for each label.
	for _, label := range labels {
		reqs, ok := s.required[label]
		if !ok {
			continue
		}
		for propName := range reqs {
			if _, present := props[propName]; !present {
				return fmt.Errorf("%w: label %q requires property %q",
					ErrMissingRequired, label, propName)
			}
		}
	}

	// Validate kinds of present properties.
	for propName, value := range props {
		d, ok := s.properties[propName]
		if !ok {
			// Unknown properties pass through; callers may use Validate
			// directly if they want strict key checking.
			continue
		}
		if value.Kind() != d.kind {
			return fmt.Errorf("%w: %q declared %d, value is %d",
				ErrTypeMismatch, propName, d.kind, value.Kind())
		}
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
