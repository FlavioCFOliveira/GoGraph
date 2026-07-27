//go:build r4audit

package r4audit

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestStringOrdering probes ORDER BY over strings that separate the two
// plausible collations: UTF-8 byte order (== Unicode code-point order, what Go's
// `<` gives) and UTF-16 code-unit order (what Java's String.compareTo, and
// therefore Neo4j, gives). They disagree whenever a supplementary-plane
// character (>= U+10000, encoded in UTF-16 as a surrogate pair in D800..DFFF) is
// compared against a BMP character in E000..FFFF.
func TestStringOrdering(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	// U+1F600 GRINNING FACE (supplementary) vs U+FB01 LATIN SMALL LIGATURE FI (BMP, > D800)
	vals := []string{"\U0001F600", "ﬁ", "z", "a", "Z", "é" /* é */, "e"}
	for i, v := range vals {
		key := fmt.Sprintf("s%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(key, "S"); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeProperty(key, "v", lpg.StringValue(v)); err != nil {
			t.Fatal(err)
		}
	}
	eng := cypher.NewEngine(g)
	res, err := eng.RunAny(context.Background(), `MATCH (n:S) RETURN n.v AS v ORDER BY n.v`, nil)
	if err != nil {
		t.Fatalf("order by: %v", err)
	}
	var got []string
	for res.Next() {
		v := res.ValueAt(0)
		s, ok := v.(expr.StringValue)
		if !ok {
			t.Fatalf("expected StringValue, got %T", v)
		}
		got = append(got, string(s))
	}
	_ = res.Close()

	// Reference orderings.
	codepoint := append([]string(nil), vals...)
	sort.Slice(codepoint, func(i, j int) bool { return codepoint[i] < codepoint[j] })
	utf16order := append([]string(nil), vals...)
	sort.Slice(utf16order, func(i, j int) bool { return cmpUTF16(utf16order[i], utf16order[j]) < 0 })

	fmt.Printf("gograph    : %s\n", esc(got))
	fmt.Printf("codepoint  : %s\n", esc(codepoint))
	fmt.Printf("utf16 (JVM): %s\n", esc(utf16order))
	fmt.Printf("gograph == codepoint : %v\n", eq(got, codepoint))
	fmt.Printf("gograph == utf16     : %v\n", eq(got, utf16order))
}

func cmpUTF16(a, b string) int {
	ua, ub := []rune(a), []rune(b)
	toU16 := func(rs []rune) []uint16 {
		var out []uint16
		for _, r := range rs {
			if r >= 0x10000 {
				r -= 0x10000
				out = append(out, uint16(0xD800+(r>>10)), uint16(0xDC00+(r&0x3FF)))
			} else {
				out = append(out, uint16(r))
			}
		}
		return out
	}
	x, y := toU16(ua), toU16(ub)
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			if x[i] < y[i] {
				return -1
			}
			return 1
		}
	}
	return len(x) - len(y)
}

func esc(ss []string) string {
	var b []string
	for _, s := range ss {
		b = append(b, fmt.Sprintf("%+q", s))
	}
	return strings.Join(b, " ")
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestParamCoercion probes which Go parameter types RunAny accepts.
func TestParamCoercion(t *testing.T) {
	eng := newEng(t, 10)
	cases := []struct {
		name string
		val  any
	}{
		{"int", 42},
		{"int64", int64(42)},
		{"float64", 3.5},
		{"string", "x"},
		{"bool", true},
		{"nil", nil},
		{"[]any", []any{1, 2}},
		{"[]int", []int{1, 2}},
		{"[]string", []string{"a"}},
		{"map[string]any", map[string]any{"k": 1}},
		{"[]map[string]any", []map[string]any{{"k": 1}}},
		{"[]byte", []byte{1, 2}},
		{"uint64", uint64(7)},
		{"int32", int32(7)},
		{"float32", float32(1.5)},
	}
	for _, c := range cases {
		_, err := eng.RunAny(context.Background(), `RETURN $p AS p`, map[string]any{"p": c.val})
		if err != nil {
			fmt.Printf("%-18s REJECT %v\n", c.name, err)
			continue
		}
		fmt.Printf("%-18s ACCEPT\n", c.name)
	}
}

// TestShowProjection checks that SHOW INDEXES actually projects values, which
// round 3 saw render as <nil> and did not chase down.
func TestShowProjection(t *testing.T) {
	eng := newEng(t, 10)
	if _, err := eng.RunAny(context.Background(), `CREATE INDEX p_name FOR (n:P) ON (n.name)`, nil); err != nil {
		t.Fatalf("create index: %v", err)
	}
	for _, q := range []string{
		`SHOW INDEXES`,
		`SHOW INDEXES YIELD name`,
		`SHOW INDEXES YIELD name, labelsOrTypes WHERE name = 'p_name'`,
	} {
		res, err := eng.RunAny(context.Background(), q, nil)
		if err != nil {
			fmt.Printf("%-52s REJECT %v\n", q, err)
			continue
		}
		cols := res.Columns()
		n := 0
		var first []string
		for res.Next() {
			if n == 0 {
				for i := range cols {
					first = append(first, fmt.Sprintf("%s=%v(%T)", cols[i], res.ValueAt(i), res.ValueAt(i)))
				}
			}
			n++
		}
		e := res.Err()
		_ = res.Close()
		fmt.Printf("%-52s rows=%d cols=%v first=[%s] err=%v\n", q, n, cols, strings.Join(first, " "), e)
	}
}
