package expr

import "testing"

func TestHashRow_EmptyRowReturnsOffset(t *testing.T) {
	got := HashRow(nil)
	const fnvOffset uint64 = 14695981039346656037
	if got != fnvOffset {
		t.Fatalf("HashRow(nil) = %d, want offset basis %d", got, fnvOffset)
	}
	if HashRow([]Value{}) != fnvOffset {
		t.Fatal("HashRow(empty) must equal HashRow(nil)")
	}
}

func TestHashRow_Deterministic(t *testing.T) {
	row := []Value{IntegerValue(1), StringValue("a"), BoolValue(true)}
	first := HashRow(row)
	for i := 0; i < 16; i++ {
		if HashRow(row) != first {
			t.Fatalf("HashRow not deterministic at iter %d", i)
		}
	}
}

func TestHashRow_EqualRowsHashEqual(t *testing.T) {
	rowA := []Value{IntegerValue(42), StringValue("hello"), FloatValue(3.14)}
	rowB := []Value{IntegerValue(42), StringValue("hello"), FloatValue(3.14)}
	if HashRow(rowA) != HashRow(rowB) {
		t.Fatalf("equal rows must hash equal: A=%d B=%d", HashRow(rowA), HashRow(rowB))
	}
}

func TestHashRow_OrderSensitive(t *testing.T) {
	a := []Value{IntegerValue(1), IntegerValue(2)}
	b := []Value{IntegerValue(2), IntegerValue(1)}
	if HashRow(a) == HashRow(b) {
		t.Fatalf("HashRow should be order-sensitive but [1,2] and [2,1] collide at %d", HashRow(a))
	}
}

func TestHashRow_LengthSensitive(t *testing.T) {
	one := []Value{IntegerValue(7)}
	two := []Value{IntegerValue(7), IntegerValue(7)}
	if HashRow(one) == HashRow(two) {
		t.Fatalf("HashRow should distinguish length but [7] and [7,7] collide at %d", HashRow(one))
	}
}

func TestHashRow_DifferentValuesDifferentHash(t *testing.T) {
	cases := []struct {
		name string
		a, b []Value
	}{
		{"int", []Value{IntegerValue(1)}, []Value{IntegerValue(2)}},
		{"string", []Value{StringValue("a")}, []Value{StringValue("b")}},
		{"bool", []Value{BoolValue(true)}, []Value{BoolValue(false)}},
		{"float", []Value{FloatValue(1.0)}, []Value{FloatValue(2.0)}},
		{"mixed", []Value{IntegerValue(1), StringValue("a")}, []Value{IntegerValue(1), StringValue("b")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if HashRow(tc.a) == HashRow(tc.b) {
				t.Fatalf("rows %v and %v collided at %d", tc.a, tc.b, HashRow(tc.a))
			}
		})
	}
}

func TestHashRow_NullParticipates(t *testing.T) {
	withNull := []Value{IntegerValue(1), Null, IntegerValue(3)}
	without := []Value{IntegerValue(1), IntegerValue(3)}
	if HashRow(withNull) == HashRow(without) {
		t.Fatal("NULL element must change the hash")
	}
}
