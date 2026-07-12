package ir

// ddl_reserved_name_test.go — regression for #1912: a user CREATE/DROP INDEX
// may not name the "__uniq__" UNIQUE-constraint backing index.

import (
	"strings"
	"testing"
)

func TestParseDDL_RejectsReservedUniqueBackingName(t *testing.T) {
	cases := []string{
		`CREATE INDEX __uniq__Person.email FOR (n:Person) ON (n.email)`,
		`CREATE INDEX __UNIQ__Person.email FOR (n:Person) ON (n.email)`, // case-insensitive
		`DROP INDEX __uniq__Person.email`,
		`DROP INDEX __uniq__Person.email IF EXISTS`,
	}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			_, err := ParseDDL(q)
			if err == nil {
				t.Fatalf("expected rejection of reserved __uniq__ name, got nil")
			}
			if !strings.Contains(err.Error(), reservedUniqueBackingPrefix) {
				t.Fatalf("error %q does not mention the reserved prefix", err.Error())
			}
		})
	}
}
