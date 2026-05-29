package proto_test

// example_test.go — runnable godoc examples for the Bolt v5 message codec
// (#1120). They show a request and a response message round-tripping through
// the PackStream encoder/decoder.

import (
	"bytes"
	"fmt"

	"gograph/bolt/packstream"
	"gograph/bolt/proto"
)

// ExampleEncodeRequest round-trips a RUN request. EncodeRequest serialises the
// message to PackStream; DecodeRequest reconstructs the concrete *proto.Run,
// preserving the query text and parameters.
func ExampleEncodeRequest() {
	msg := &proto.Run{
		Query:      "MATCH (n) RETURN n",
		Parameters: map[string]packstream.Value{"limit": int64(10)},
	}

	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := proto.EncodeRequest(enc, msg); err != nil {
		fmt.Println("encode:", err)
		return
	}
	if err := enc.Flush(); err != nil {
		fmt.Println("flush:", err)
		return
	}

	dec := packstream.NewDecoder(&buf)
	decoded, err := proto.DecodeRequest(dec)
	if err != nil {
		fmt.Println("decode:", err)
		return
	}

	run := decoded.(*proto.Run)
	fmt.Println("query:", run.Query)
	fmt.Println("limit:", run.Parameters["limit"])
	// Output:
	// query: MATCH (n) RETURN n
	// limit: 10
}

// ExampleEncodeResponse round-trips a SUCCESS response carrying server
// metadata. The decoded *proto.Success exposes the same metadata map.
func ExampleEncodeResponse() {
	msg := &proto.Success{
		Metadata: map[string]packstream.Value{"server": "GoGraph/1.0"},
	}

	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := proto.EncodeResponse(enc, msg); err != nil {
		fmt.Println("encode:", err)
		return
	}
	if err := enc.Flush(); err != nil {
		fmt.Println("flush:", err)
		return
	}

	dec := packstream.NewDecoder(&buf)
	decoded, err := proto.DecodeResponse(dec)
	if err != nil {
		fmt.Println("decode:", err)
		return
	}

	success := decoded.(*proto.Success)
	fmt.Printf("server: %v\n", success.Metadata["server"])
	// Output:
	// server: GoGraph/1.0
}
