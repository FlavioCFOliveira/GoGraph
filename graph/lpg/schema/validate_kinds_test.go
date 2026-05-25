package schema

import (
	"errors"
	"math"
	"testing"
	"time"

	"gograph/graph/lpg"
)

// TestSchema_ValidateKinds exercises schema.Validate for every
// PropertyKind: happy path (3 well-formed values) and rejection path
// (3 values of a wrong kind), plus ErrUnknownProperty.
func TestSchema_ValidateKinds(t *testing.T) {
	t.Parallel()

	type kindCase struct {
		name   string
		kind   lpg.PropertyKind
		accept []lpg.PropertyValue // must pass Validate
		reject []lpg.PropertyValue // must return ErrTypeMismatch
	}

	cases := []kindCase{
		{
			name: "PropString",
			kind: lpg.PropString,
			accept: []lpg.PropertyValue{
				lpg.StringValue(""),
				lpg.StringValue("hello"),
				lpg.StringValue("∞ unicode"),
			},
			reject: []lpg.PropertyValue{
				lpg.Int64Value(0),
				lpg.Float64Value(0.0),
				lpg.BoolValue(false),
			},
		},
		{
			name: "PropInt64",
			kind: lpg.PropInt64,
			accept: []lpg.PropertyValue{
				lpg.Int64Value(0),
				lpg.Int64Value(math.MinInt64),
				lpg.Int64Value(math.MaxInt64),
			},
			reject: []lpg.PropertyValue{
				lpg.StringValue("0"),
				lpg.Float64Value(0.0),
				lpg.BoolValue(true),
			},
		},
		{
			name: "PropFloat64",
			kind: lpg.PropFloat64,
			accept: []lpg.PropertyValue{
				lpg.Float64Value(0.0),
				lpg.Float64Value(math.Pi),
				lpg.Float64Value(math.MaxFloat64),
			},
			reject: []lpg.PropertyValue{
				lpg.StringValue("3.14"),
				lpg.Int64Value(3),
				lpg.BoolValue(false),
			},
		},
		{
			name: "PropBool",
			kind: lpg.PropBool,
			accept: []lpg.PropertyValue{
				lpg.BoolValue(true),
				lpg.BoolValue(false),
				lpg.BoolValue(false),
			},
			reject: []lpg.PropertyValue{
				lpg.StringValue("true"),
				lpg.Int64Value(1),
				lpg.Float64Value(1.0),
			},
		},
		{
			name: "PropTime",
			kind: lpg.PropTime,
			accept: []lpg.PropertyValue{
				lpg.TimeValue(time.Now().UTC()),
				lpg.TimeValue(time.Unix(0, 0).UTC()),
				lpg.TimeValue(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
			},
			reject: []lpg.PropertyValue{
				lpg.StringValue("2026-01-01"),
				lpg.Int64Value(0),
				lpg.BoolValue(false),
			},
		},
		{
			name: "PropBytes",
			kind: lpg.PropBytes,
			accept: []lpg.PropertyValue{
				lpg.BytesValue(nil),
				lpg.BytesValue([]byte{}),
				lpg.BytesValue([]byte{0x00, 0xFF}),
			},
			reject: []lpg.PropertyValue{
				lpg.StringValue("bytes"),
				lpg.Int64Value(0),
				lpg.Float64Value(0.0),
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := New(nil, nil)
			if _, err := s.RegisterProperty("prop", tc.kind); err != nil {
				t.Fatalf("RegisterProperty: %v", err)
			}

			for i, v := range tc.accept {
				if err := s.Validate("prop", v); err != nil {
					t.Errorf("accept[%d]: unexpected error: %v", i, err)
				}
			}

			for i, v := range tc.reject {
				err := s.Validate("prop", v)
				if !errors.Is(err, ErrTypeMismatch) {
					t.Errorf("reject[%d]: got %v, want ErrTypeMismatch", i, err)
				}
			}
		})
	}

	t.Run("ErrUnknownProperty", func(t *testing.T) {
		t.Parallel()
		s := New(nil, nil)
		err := s.Validate("unregistered", lpg.Int64Value(1))
		if !errors.Is(err, ErrUnknownProperty) {
			t.Fatalf("got %v, want ErrUnknownProperty", err)
		}
	})
}
