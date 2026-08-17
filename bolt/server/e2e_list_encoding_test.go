package server_test

// e2e_list_encoding_test.go — the negotiated Bolt version reaches list ELEMENTS
// (rmp #2513).
//
// A PackStream List is version-INDEPENDENT: the same markers (0x90 TinyList,
// 0xD4/D5/D6) carry it in PackStream v1 (Bolt <=4.x) and v2 (Bolt 4.4/5.x), and
// this module's packstream codec takes no version parameter at all — the
// neo4j-go-driver's own unpacker is likewise a single version-free codec, with
// all version branching confined to its struct hydrator. What DOES branch on the
// version are the ELEMENTS: a Node carries element_id only on Bolt 5+, and a
// DateTime takes a different struct tag on 4.4.
//
// So the list arm needs no version-specific representation, but it must thread
// boltMajor into every element. The encoder-level matrix in list_value_test.go
// calls exprValueToPackstream with an explicit version; this test proves the
// version the SESSION negotiated is the one that actually arrives, over a real
// socket, on both supported majors.

import (
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// TestE2E_ListOfNodesCarriesNegotiatedVersion runs nodes(p) over a real
// connection on each supported Bolt major and asserts the Node structures
// INSIDE the list carry that major's field count — 3 on Bolt 4.4, 4 on Bolt 5.
// A list arm that dropped boltMajor would be invisible on 5.x and wrong on 4.4.
func TestE2E_ListOfNodesCarriesNegotiatedVersion(t *testing.T) {
	addr := startTestServer(t, server.Options{ConnTimeout: 10 * time.Second})

	for _, tc := range []struct {
		name       string
		create     string
		major      uint8
		minor      uint8
		wantFields int
	}{
		{"bolt 4.4", "CREATE (:E2EList {v: 44})", 4, 4, 3},
		{"bolt 5.0", "CREATE (:E2EList {v: 50})", 5, 0, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newBoltTestClient(t, addr)
			defer c.close(t)

			ver := c.negotiateVersion(t, tc.major, tc.minor)
			if ver.Major != tc.major {
				t.Fatalf("negotiated %d.%d, want major %d", ver.Major, ver.Minor, tc.major)
			}
			c.hello(t)

			c.run(t, tc.create, nil)
			c.pullAll(t)

			c.run(t, "MATCH (n:E2EList) RETURN collect(n) AS ns, [1, 2, 3] AS lit", nil)
			records, _ := c.pullAll(t)
			if len(records) != 1 || len(records[0]) != 2 {
				t.Fatalf("expected two columns in one row, got %d row(s)", len(records))
			}

			// collect(n) — a List of Node structures, not a String.
			ns, ok := records[0][0].([]packstream.Value)
			if !ok {
				t.Fatalf("collect(n): got %T %#v, want a packstream List", records[0][0], records[0][0])
			}
			if len(ns) == 0 {
				t.Fatal("collect(n): empty")
			}
			node, ok := ns[0].(packstream.Struct)
			if !ok {
				t.Fatalf("collect(n)[0]: got %T %#v, want a Node Struct", ns[0], ns[0])
			}
			if node.Tag != 0x4E {
				t.Errorf("collect(n)[0]: tag 0x%02X, want 0x4E ('N')", node.Tag)
			}
			if len(node.Fields) != tc.wantFields {
				t.Errorf("collect(n)[0]: %d fields, want %d on Bolt %d.%d — the negotiated version "+
					"did not reach the list element", len(node.Fields), tc.wantFields, tc.major, tc.minor)
			}

			// A literal list is version-independent on both majors.
			lit, ok := records[0][1].([]packstream.Value)
			if !ok {
				t.Fatalf("literal list: got %T %#v, want a packstream List", records[0][1], records[0][1])
			}
			if len(lit) != 3 || lit[0] != int64(1) || lit[2] != int64(3) {
				t.Errorf("literal list: got %#v, want [1 2 3]", lit)
			}

			c.run(t, "MATCH (n:E2EList) DELETE n", nil)
			c.pullAll(t)
		})
	}
}
