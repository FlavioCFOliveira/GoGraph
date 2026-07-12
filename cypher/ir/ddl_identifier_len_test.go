package ir

// ddl_identifier_len_test.go — regression for #1903: the DDL parser must reject
// a CREATE/DROP INDEX or CREATE/DROP CONSTRAINT identifier (name, label,
// property) longer than maxSchemaIdentifierLen, so an over-long identifier
// cannot reach the durable layer where it would truncate in the uint16 WAL
// prefix or be written wider than the snapshot reader accepts.

import (
	"strings"
	"testing"
)

func TestParseDDL_RejectsOverLongIdentifier(t *testing.T) {
	long := strings.Repeat("a", maxSchemaIdentifierLen+1)
	atCap := strings.Repeat("a", maxSchemaIdentifierLen)

	reject := []struct {
		name  string
		query string
	}{
		{"constraint name", `CREATE CONSTRAINT ` + long + ` ON (n:Label) ASSERT n.p IS UNIQUE`},
		{"constraint label", `CREATE CONSTRAINT c ON (n:` + long + `) ASSERT n.p IS UNIQUE`},
		{"constraint property", `CREATE CONSTRAINT c ON (n:Label) ASSERT n.` + long + ` IS NOT NULL`},
		{"index name", `CREATE INDEX ` + long + ` FOR (n:Label) ON (n.p)`},
		{"index label", `CREATE INDEX i FOR (n:` + long + `) ON (n.p)`},
		{"index property", `CREATE INDEX i FOR (n:Label) ON (n.` + long + `)`},
		{"drop constraint name", `DROP CONSTRAINT ` + long},
		{"drop index name", `DROP INDEX ` + long},
	}
	for _, tc := range reject {
		t.Run("reject/"+tc.name, func(t *testing.T) {
			if _, err := ParseDDL(tc.query); err == nil {
				t.Fatalf("expected error for over-long %s, got nil", tc.name)
			}
		})
	}

	// An identifier exactly at the cap must be accepted (boundary is inclusive).
	accept := []struct {
		name  string
		query string
	}{
		{"constraint name at cap", `CREATE CONSTRAINT ` + atCap + ` ON (n:Label) ASSERT n.p IS UNIQUE`},
		{"index name at cap", `CREATE INDEX ` + atCap + ` FOR (n:Label) ON (n.p)`},
	}
	for _, tc := range accept {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			if _, err := ParseDDL(tc.query); err != nil {
				t.Fatalf("expected at-cap %s to be accepted, got: %v", tc.name, err)
			}
		})
	}
}
