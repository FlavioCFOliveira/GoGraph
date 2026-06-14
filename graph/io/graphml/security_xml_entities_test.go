package graphml_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	graphml "github.com/FlavioCFOliveira/GoGraph/graph/io/graphml"
)

// Security regression battery for the GraphML reader's resistance to the
// classic hostile-XML attacks: XML External Entity (CWE-611), the
// "billion laughs" internal-entity expansion bomb (CWE-776), external DTD
// fetches, and unbounded element nesting (CWE-674).
//
// The GraphML reader decodes with the standard library's encoding/xml,
// which by design never resolves custom internal entities, never fetches
// external entities or DTDs, and never follows SYSTEM identifiers. These
// tests pin that property: they pass today and must keep passing, so that
// a future switch to a different XML decoder cannot silently reintroduce
// an entity-expansion or file-disclosure vector.
//
// All crafted inputs are kept to a few kilobytes; they prove the boundary
// by construction (an undefined-entity reference is rejected before any
// expansion), not by trying to allocate a large payload.

// secIOSecretMarker is the canary string written to a temp file by the
// XXE test. If the reader ever resolved the external entity, this exact
// string would surface inside the parsed graph.
const secIOSecretMarker = "TOP-SECRET-CANARY-2f9c1a7e"

// secIOWriteSecretFile writes secIOSecretMarker to a fresh temp file under
// t.TempDir and returns its absolute path. The file is cleaned up
// automatically when the test ends.
func secIOWriteSecretFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(path, []byte(secIOSecretMarker), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	return path
}

// TestSec_IO_GraphMLReadRepelsXXE fires an XML External Entity payload that
// references a local file via a SYSTEM entity and expands it inside a node
// id. The reader must (a) fail with a parse error, (b) never panic, and
// (c) never let the secret file's contents reach the caller — neither in
// an error nor (if it somehow parsed) in any node id.
func TestSec_IO_GraphMLReadRepelsXXE(t *testing.T) {
	t.Parallel()

	secret := secIOWriteSecretFile(t)
	doc := `<?xml version="1.0"?>` +
		`<!DOCTYPE graphml [ <!ENTITY xxe SYSTEM "file://` + secret + `"> ]>` +
		`<graphml xmlns="http://graphml.graphdrawing.org/xmlns">` +
		`<graph edgedefault="directed">` +
		`<node id="&xxe;"/>` +
		`</graph></graphml>`

	a, n, err := secIOReadGraphML(t, doc)
	if err == nil {
		t.Fatalf("XXE document accepted: want a parse error, got graph=%v edges=%d", a, n)
	}
	if a != nil {
		t.Errorf("graph = %v, want nil on error", a)
	}
	// The secret must never leak — not via the resolved entity, not via the
	// error text. encoding/xml rejects the undefined entity before any I/O.
	if err != nil && strings.Contains(err.Error(), secIOSecretMarker) {
		t.Errorf("secret file content leaked into error: %v", err)
	}
}

// TestSec_IO_GraphMLReadRepelsXXE_WithProps is the typed-property analogue:
// the ReadWithProps path uses the same decoder and must repel XXE the same
// way. The auditor flagged the typed-property read path as the one lacking
// an explicit hostile-XML regression pin.
func TestSec_IO_GraphMLReadRepelsXXE_WithProps(t *testing.T) {
	t.Parallel()

	secret := secIOWriteSecretFile(t)
	// Expand the entity inside a typed <data> value so the property decode
	// path would be the one to observe a leak, were the entity resolved.
	doc := `<?xml version="1.0"?>` +
		`<!DOCTYPE graphml [ <!ENTITY xxe SYSTEM "file://` + secret + `"> ]>` +
		`<graphml xmlns="http://graphml.graphdrawing.org/xmlns">` +
		`<key id="p_s" for="node" attr.name="s" attr.type="string"/>` +
		`<graph edgedefault="directed">` +
		`<node id="n0"><data key="p_s">&xxe;</data></node>` +
		`</graph></graphml>`

	g, _, err := graphml.ReadWithProps(strings.NewReader(doc))
	if err == nil {
		t.Fatalf("XXE document accepted by ReadWithProps: want a parse error")
	}
	if g != nil {
		t.Errorf("graph = %v, want nil on error", g)
	}
	if strings.Contains(err.Error(), secIOSecretMarker) {
		t.Errorf("secret file content leaked into error: %v", err)
	}
}

// TestSec_IO_GraphMLReadRepelsBillionLaughs fires the canonical nested
// internal-entity expansion bomb (&a;→&b;→&c;→&d;). encoding/xml does not
// expand custom internal entities at all, so the reference to &d; is
// rejected as an undefined entity — there is no exponential expansion, no
// memory blow-up, and no panic. Kept tiny on the wire by construction.
func TestSec_IO_GraphMLReadRepelsBillionLaughs(t *testing.T) {
	t.Parallel()

	doc := `<?xml version="1.0"?>` +
		`<!DOCTYPE lolz [` +
		`<!ENTITY a "dos">` +
		`<!ENTITY b "&a;&a;&a;&a;&a;&a;&a;&a;&a;&a;">` +
		`<!ENTITY c "&b;&b;&b;&b;&b;&b;&b;&b;&b;&b;">` +
		`<!ENTITY d "&c;&c;&c;&c;&c;&c;&c;&c;&c;&c;">` +
		`]>` +
		`<graphml xmlns="http://graphml.graphdrawing.org/xmlns">` +
		`<graph edgedefault="directed">` +
		`<node id="&d;"/>` +
		`</graph></graphml>`

	a, _, err := secIOReadGraphML(t, doc)
	if err == nil {
		t.Fatalf("billion-laughs document accepted: want an undefined-entity error")
	}
	if a != nil {
		t.Errorf("graph = %v, want nil on error", a)
	}
}

// TestSec_IO_GraphMLReadIgnoresExternalDTD points the document's DOCTYPE at
// a non-existent local DTD via a SYSTEM identifier. encoding/xml never
// fetches the external subset, so the parse neither hangs nor errors on
// the missing file: the well-formed document below the DOCTYPE parses
// normally and the forbidden path is simply ignored.
func TestSec_IO_GraphMLReadIgnoresExternalDTD(t *testing.T) {
	t.Parallel()

	// A path that must never be opened. If the reader fetched it the test
	// would either error (file missing) or, worse, read it.
	doc := `<?xml version="1.0"?>` +
		`<!DOCTYPE graphml SYSTEM "file:///nonexistent/forbidden-` + secIOSecretMarker + `.dtd">` +
		`<graphml xmlns="http://graphml.graphdrawing.org/xmlns">` +
		`<graph edgedefault="directed">` +
		`<node id="a"/><node id="b"/>` +
		`<edge source="a" target="b"/>` +
		`</graph></graphml>`

	a, n, err := secIOReadGraphML(t, doc)
	if err != nil {
		t.Fatalf("external-DTD document rejected: err=%v, want nil (DTD ignored, body parsed)", err)
	}
	if a == nil {
		t.Fatal("graph is nil; external DTD must be ignored and the body parsed")
	}
	if n != 1 {
		t.Errorf("edges = %d, want 1", n)
	}
}

// TestSec_IO_GraphMLReadBoundedDeepNesting feeds a deeply nested run of
// <x> elements (a few hundred KiB, well under the byte cap) and asserts the
// decoder returns without a stack-overflow panic. encoding/xml is iterative
// for element nesting, so the document is rejected as not matching the
// expected <graphml> root rather than crashing. The guard here is "no
// panic / bounded return", not a specific error.
func TestSec_IO_GraphMLReadBoundedDeepNesting(t *testing.T) {
	t.Parallel()

	const depth = 50_000 // ~150 KiB of "<x>" + "</x>"; far below the byte cap
	var b strings.Builder
	b.Grow(depth * 7)
	b.WriteString(`<?xml version="1.0"?>`)
	for i := 0; i < depth; i++ {
		b.WriteString("<x>")
	}
	for i := 0; i < depth; i++ {
		b.WriteString("</x>")
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("deeply nested document panicked: %v", r)
		}
	}()

	// Cap explicitly so a pathological input can never read without end.
	a, _, err := graphml.ReadIntoCappedCtx(context.Background(),
		strings.NewReader(b.String()), graphml.DefaultMaxBytes)
	// The root is <x>, not <graphml>: a typed parse error, no graph, no panic.
	if err == nil {
		t.Errorf("deeply nested non-graphml root accepted: want a parse error")
	}
	if a != nil {
		t.Errorf("graph = %v, want nil on error", a)
	}
}

// secIOReadGraphML runs the plain ReadInto path under an explicit
// DefaultMaxBytes cap and recovers any panic into a fatal test failure, so
// every caller above gets the same "never crashes the host" guarantee for
// free.
func secIOReadGraphML(t *testing.T, doc string) (a *adjlist.AdjList[string, int64], edges int, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("GraphML reader panicked on hostile input: %v", r)
		}
	}()
	return graphml.ReadIntoCappedCtx(context.Background(),
		strings.NewReader(doc), graphml.DefaultMaxBytes)
}
