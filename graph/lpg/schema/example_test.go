package schema_test

import (
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg/schema"
)

// ExampleSchema declares property typing and a per-label requirement,
// then validates candidate nodes against the declaration. Validation is
// opt-in: callers run it before applying a write to reject incompatible
// data early.
func ExampleSchema() {
	s := schema.New(lpg.NewLabelRegistry(), lpg.NewPropertyKeyRegistry())

	// Declare that "Person" exists and that "age" is an Int64 property
	// every Person must carry.
	s.RegisterLabel("Person")
	_, _ = s.RegisterProperty("age", lpg.PropInt64)
	s.RequireProperty("Person", "age")

	// A Person with a correctly typed age satisfies the schema.
	ok := s.ValidateNode(
		[]string{"Person"},
		map[string]lpg.PropertyValue{"age": lpg.Int64Value(30)},
	)

	// A Person missing the required "age" fails with ErrMissingRequired.
	missing := s.ValidateNode([]string{"Person"}, map[string]lpg.PropertyValue{})

	fmt.Println("valid node:", ok)
	fmt.Println("missing required:", errors.Is(missing, schema.ErrMissingRequired))
	// Output:
	// valid node: <nil>
	// missing required: true
}

// ExampleSchema_Validate checks a single value against the declared
// kind of its property key, reporting ErrTypeMismatch when the kinds
// disagree and ErrUnknownProperty when the key was never registered.
func ExampleSchema_Validate() {
	s := schema.New(lpg.NewLabelRegistry(), lpg.NewPropertyKeyRegistry())
	_, _ = s.RegisterProperty("age", lpg.PropInt64)

	good := s.Validate("age", lpg.Int64Value(42))
	wrongType := s.Validate("age", lpg.StringValue("not a number"))
	unknown := s.Validate("nickname", lpg.StringValue("ace"))

	fmt.Println("int64 ok:", good)
	fmt.Println("type mismatch:", errors.Is(wrongType, schema.ErrTypeMismatch))
	fmt.Println("unknown key:", errors.Is(unknown, schema.ErrUnknownProperty))
	// Output:
	// int64 ok: <nil>
	// type mismatch: true
	// unknown key: true
}
