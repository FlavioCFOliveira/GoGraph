package packstream_test

// example_test.go — runnable godoc examples for the PackStream codec (#1121).
// They show a value round-trip through Encoder/Decoder and the wire bytes of
// a small encoding.

import (
	"bytes"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
)

// ExampleEncoder_WriteValue round-trips a heterogeneous list through the
// Value-level helpers. WriteValue dispatches on the Go type; ReadValue
// reconstructs the same values (integers come back as int64, strings as
// string), so the decoded list equals the input.
func ExampleEncoder_WriteValue() {
	in := []packstream.Value{int64(42), "go", true}

	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := enc.WriteValue(in); err != nil {
		fmt.Println("write:", err)
		return
	}
	if err := enc.Flush(); err != nil {
		fmt.Println("flush:", err)
		return
	}

	dec := packstream.NewDecoder(&buf)
	out, err := dec.ReadValue()
	if err != nil {
		fmt.Println("read:", err)
		return
	}

	list := out.([]packstream.Value)
	fmt.Println("len:", len(list))
	fmt.Printf("values: %v %q %v\n", list[0], list[1], list[2])
	// Output:
	// len: 3
	// values: 42 "go" true
}

// ExampleEncoder_WriteInt shows the wire form of a TINY_INT. PackStream encodes
// any integer in the range -16..127 as a single byte equal to its two's
// complement, so 42 serialises to one byte: 0x2a.
func ExampleEncoder_WriteInt() {
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := enc.WriteInt(42); err != nil {
		fmt.Println("write:", err)
		return
	}
	if err := enc.Flush(); err != nil {
		fmt.Println("flush:", err)
		return
	}

	fmt.Printf("bytes: % x\n", buf.Bytes())
	// Output:
	// bytes: 2a
}
