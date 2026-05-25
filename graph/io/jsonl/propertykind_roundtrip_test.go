package jsonl_test

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	"gograph/graph/adjlist"
	"gograph/graph/io/jsonl"
	"gograph/graph/lpg"
)

// TestJSONL_PropertyKindRoundtrip verifies that all six PropertyKind
// variants survive a WriteWithProps→ReadWithProps roundtrip without
// loss of kind or value.
func TestJSONL_PropertyKindRoundtrip(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)

	cases := []struct {
		name  string
		value lpg.PropertyValue
		check func(t *testing.T, got lpg.PropertyValue)
	}{
		{
			name:  "string",
			value: lpg.StringValue("hello world"),
			check: func(t *testing.T, got lpg.PropertyValue) {
				t.Helper()
				if got.Kind() != lpg.PropString {
					t.Fatalf("kind = %v, want PropString", got.Kind())
				}
				s, _ := got.String()
				if s != "hello world" {
					t.Fatalf("value = %q, want \"hello world\"", s)
				}
			},
		},
		{
			name:  "int64",
			value: lpg.Int64Value(math.MinInt64),
			check: func(t *testing.T, got lpg.PropertyValue) {
				t.Helper()
				if got.Kind() != lpg.PropInt64 {
					t.Fatalf("kind = %v, want PropInt64", got.Kind())
				}
				i, _ := got.Int64()
				if i != math.MinInt64 {
					t.Fatalf("value = %d, want %d", i, int64(math.MinInt64))
				}
			},
		},
		{
			name:  "float64",
			value: lpg.Float64Value(3.14159265358979),
			check: func(t *testing.T, got lpg.PropertyValue) {
				t.Helper()
				if got.Kind() != lpg.PropFloat64 {
					t.Fatalf("kind = %v, want PropFloat64", got.Kind())
				}
				f, _ := got.Float64()
				if f != 3.14159265358979 {
					t.Fatalf("value = %v, want 3.14159265358979", f)
				}
			},
		},
		{
			name:  "bool_true",
			value: lpg.BoolValue(true),
			check: func(t *testing.T, got lpg.PropertyValue) {
				t.Helper()
				if got.Kind() != lpg.PropBool {
					t.Fatalf("kind = %v, want PropBool", got.Kind())
				}
				b, _ := got.Bool()
				if !b {
					t.Fatal("value = false, want true")
				}
			},
		},
		{
			name:  "bool_false",
			value: lpg.BoolValue(false),
			check: func(t *testing.T, got lpg.PropertyValue) {
				t.Helper()
				if got.Kind() != lpg.PropBool {
					t.Fatalf("kind = %v, want PropBool", got.Kind())
				}
				b, _ := got.Bool()
				if b {
					t.Fatal("value = true, want false")
				}
			},
		},
		{
			name:  "time",
			value: lpg.TimeValue(now),
			check: func(t *testing.T, got lpg.PropertyValue) {
				t.Helper()
				if got.Kind() != lpg.PropTime {
					t.Fatalf("kind = %v, want PropTime", got.Kind())
				}
				ts, _ := got.Time()
				if !ts.Equal(now) {
					t.Fatalf("value = %v, want %v", ts, now)
				}
			},
		},
		{
			name:  "bytes",
			value: lpg.BytesValue([]byte{0x00, 0x01, 0xFF}),
			check: func(t *testing.T, got lpg.PropertyValue) {
				t.Helper()
				if got.Kind() != lpg.PropBytes {
					t.Fatalf("kind = %v, want PropBytes", got.Kind())
				}
				b, _ := got.Bytes()
				if len(b) != 3 || b[0] != 0x00 || b[1] != 0x01 || b[2] != 0xFF {
					t.Fatalf("value = %v, want [0x00 0x01 0xFF]", b)
				}
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			g := lpg.New[string, int64](adjlist.Config{Directed: true})
			if err := g.AddNode("n"); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
			if err := g.SetNodeProperty("n", "prop", tc.value); err != nil {
				t.Fatalf("SetNodeProperty: %v", err)
			}

			var buf bytes.Buffer
			if _, err := jsonl.WriteWithProps(&buf, g); err != nil {
				t.Fatalf("WriteWithProps: %v", err)
			}

			g2, _, err := jsonl.ReadWithProps(strings.NewReader(buf.String()), adjlist.Config{Directed: true})
			if err != nil {
				t.Fatalf("ReadWithProps: %v", err)
			}

			got, ok := g2.GetNodeProperty("n", "prop")
			if !ok {
				t.Fatal("property missing after roundtrip")
			}
			tc.check(t, got)
		})
	}
}
