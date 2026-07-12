package cypher_test

// constraint_error_message_test.go — regression for #1914: a UNIQUE violation
// message must show the human value, never the internal kind-tagged dedup key
// (which carries \x00 tag bytes).

import (
	"strings"
	"testing"
)

func TestUniqueViolation_MessageHasNoEncodedKeyBytes(t *testing.T) {
	t.Parallel()
	eng := newConstraintEngine(t, "Person", "email")

	constraintMustWrite(t, eng, `CREATE (:Person {email: "a@b.com"})`)
	err := tryWrite(eng, `CREATE (:Person {email: "a@b.com"})`)
	if err == nil {
		t.Fatal("expected a UNIQUE violation")
	}
	msg := err.Error()
	if strings.ContainsRune(msg, '\x00') {
		t.Fatalf("violation message leaks internal encoded key (\\x00 bytes): %q", msg)
	}
	if !strings.Contains(msg, "a@b.com") {
		t.Fatalf("violation message should show the human value, got: %q", msg)
	}
}
