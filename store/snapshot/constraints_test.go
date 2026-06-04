package snapshot

// constraints_test.go — unit coverage for the constraints.bin component
// (#1316 checkpoint-survival half): round-trip, deterministic ordering, and
// strict corruption rejection.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestConstraints_RoundTrip(t *testing.T) {
	specs := []ConstraintSpec{
		{Kind: 1, Label: "User", Property: "email", Name: "u_email"},
		{Kind: 0, Label: "Account", Property: "login", Name: "u_login"},
		{Kind: 0, Label: "Account", Property: "id", Name: ""}, // empty name allowed
	}
	var buf bytes.Buffer
	size, crc, err := WriteConstraints(&buf, specs)
	if err != nil {
		t.Fatalf("WriteConstraints: %v", err)
	}
	if int64(buf.Len()) != size {
		t.Fatalf("reported size %d != bytes written %d", size, buf.Len())
	}
	if crc == 0 {
		t.Fatal("crc must be non-zero for a non-empty payload")
	}

	rb, err := ReadConstraints(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadConstraints: %v", err)
	}
	if len(rb.Specs) != len(specs) {
		t.Fatalf("got %d specs, want %d", len(rb.Specs), len(specs))
	}
	// Output is deterministically ordered (kind, label, property, name): kind 0
	// (UNIQUE) before kind 1 (NOT NULL); within kind 0, Account.id before
	// Account.login.
	want := []ConstraintSpec{
		{Kind: 0, Label: "Account", Property: "id", Name: ""},
		{Kind: 0, Label: "Account", Property: "login", Name: "u_login"},
		{Kind: 1, Label: "User", Property: "email", Name: "u_email"},
	}
	for i := range want {
		if rb.Specs[i] != want[i] {
			t.Errorf("spec[%d] = %+v, want %+v", i, rb.Specs[i], want[i])
		}
	}
}

func TestConstraints_DeterministicBytes(t *testing.T) {
	// Two writes of the same logical set (in different input order) must
	// produce byte-identical output, so the snapshot component is stable.
	a := []ConstraintSpec{
		{Kind: 0, Label: "B", Property: "y", Name: "n2"},
		{Kind: 1, Label: "A", Property: "x", Name: "n1"},
	}
	b := []ConstraintSpec{
		{Kind: 1, Label: "A", Property: "x", Name: "n1"},
		{Kind: 0, Label: "B", Property: "y", Name: "n2"},
	}
	var bufA, bufB bytes.Buffer
	if _, _, err := WriteConstraints(&bufA, a); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if _, _, err := WriteConstraints(&bufB, b); err != nil {
		t.Fatalf("write b: %v", err)
	}
	if !bytes.Equal(bufA.Bytes(), bufB.Bytes()) {
		t.Fatal("constraints.bin is not byte-deterministic across input orderings")
	}
}

func TestConstraints_EmptyRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if _, _, err := WriteConstraints(&buf, nil); err != nil {
		t.Fatalf("WriteConstraints(nil): %v", err)
	}
	rb, err := ReadConstraints(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadConstraints(empty): %v", err)
	}
	if len(rb.Specs) != 0 {
		t.Fatalf("got %d specs, want 0", len(rb.Specs))
	}
}

func TestConstraints_CorruptionRejected(t *testing.T) {
	good := func() []byte {
		var buf bytes.Buffer
		_, _, _ = WriteConstraints(&buf, []ConstraintSpec{{Kind: 0, Label: "L", Property: "p", Name: "n"}})
		return buf.Bytes()
	}

	t.Run("bad magic", func(t *testing.T) {
		b := good()
		b[0] ^= 0xFF
		if _, err := ReadConstraints(bytes.NewReader(b)); !errors.Is(err, ErrConstraintsCorrupted) {
			t.Fatalf("got %v, want ErrConstraintsCorrupted", err)
		}
	})

	t.Run("bad version", func(t *testing.T) {
		b := good()
		binary.LittleEndian.PutUint32(b[4:8], 99)
		if _, err := ReadConstraints(bytes.NewReader(b)); !errors.Is(err, ErrConstraintsCorrupted) {
			t.Fatalf("got %v, want ErrConstraintsCorrupted", err)
		}
	})

	t.Run("truncated record", func(t *testing.T) {
		b := good()
		b = b[:len(b)-3] // cut into the trailing name bytes
		if _, err := ReadConstraints(bytes.NewReader(b)); !errors.Is(err, ErrConstraintsCorrupted) {
			t.Fatalf("got %v, want ErrConstraintsCorrupted", err)
		}
	})

	t.Run("implausible count", func(t *testing.T) {
		b := good()
		binary.LittleEndian.PutUint32(b[8:12], constraintsMaxCount+1)
		if _, err := ReadConstraints(bytes.NewReader(b)); !errors.Is(err, ErrConstraintsCorrupted) {
			t.Fatalf("got %v, want ErrConstraintsCorrupted", err)
		}
	})
}
