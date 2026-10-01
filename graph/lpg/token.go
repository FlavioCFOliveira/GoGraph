package lpg

import (
	"errors"
	"fmt"
)

// MaxTokenLen is the largest byte length of a TOKEN — a node label, a
// relationship type, or a property key — that this module accepts: 65535
// bytes.
//
// It is the one definition of the limit (rmp #2748). The write-ahead log
// stores every token behind a uint16 length prefix, so 65535 is the most it can
// carry; store/txn derives its own bound from this constant rather than
// declaring the number again. Bounding the in-memory engine at the same value
// is what makes a graph durable or not independently of how it was built: an
// in-memory graph, a WAL-backed store, a bulk import and a Cypher statement now
// refuse exactly the same names.
//
// The limit is a BYTE length (len(name)), not a character count. A caller can
// check a name before any write with [CheckToken].
const MaxTokenLen = 1<<16 - 1

// ErrTokenTooLong is the sentinel every refusal of an over-long token wraps:
// a label, a relationship type or a property key longer than [MaxTokenLen]
// bytes. Every mutator that takes a token returns it BEFORE it changes any
// state, so a refused call leaves the graph exactly as it was.
//
// store/txn.ErrTokenTooLong is this same value, so errors.Is matches a refusal
// from either layer against either name.
var ErrTokenTooLong = errors.New("token too long")

// CheckToken reports whether name fits [MaxTokenLen], returning an error that
// wraps [ErrTokenTooLong] when it does not. what names the token in that error
// ("node label", "relationship type", "property key", ...).
//
// It is the check every token-taking mutator in this package runs first, and
// it is exported so a caller can validate a name before it writes anything.
func CheckToken(what, name string) error {
	if len(name) > MaxTokenLen {
		return errTokenTooLong(what, len(name))
	}
	return nil
}

// errTokenTooLong builds the refusal. It is separate from [CheckToken] so the
// check stays within the inliner's budget: it runs on every label and key
// write and must cost a compare and a well-predicted branch, not a call.
func errTokenTooLong(what string, n int) error {
	return fmt.Errorf("%w: %s is %d bytes, maximum %d", ErrTokenTooLong, what, n, MaxTokenLen)
}

// checkTokens is [CheckToken] over two tokens, for the mutators that take both
// a relationship type and a property key.
func checkTokens(whatA, a, whatB, b string) error {
	if err := CheckToken(whatA, a); err != nil {
		return err
	}
	return CheckToken(whatB, b)
}
