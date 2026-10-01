package server

// field_kind_codes_2942_test.go — regression gate for rmp #2942 and #2827.
//
// Before #2942 every over-long field shared Neo.ClientError.Statement.ArgumentError,
// the code substring('abc', -1) also earns, so a client could not tell an
// over-long label from a bad function argument. The code now follows the field
// kind, and the argument-error code is left to argument errors alone.
//
// #2827: store/snapshot.ErrFieldTooLong is deliberately unmapped. No Bolt
// statement can return it (only the checkpointer, bulk import and the offline
// tools write snapshots), and if one ever did it would be a server fault, so it
// must stay a masked DatabaseError rather than become a client-fault code.

import (
	"errors"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

func TestFailureCode_FieldKinds_2942(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		err        error
		want       string
		clientSide bool
	}{
		{
			name:       "token: node label",
			err:        fmt.Errorf("cypher: commit WAL: %w", fmt.Errorf("%w: node label is 65536 bytes, maximum 65535", txn.ErrTokenTooLong)),
			want:       "Neo.ClientError.Schema.TokenLengthError",
			clientSide: true,
		},
		{
			name:       "token: CheckSchemaField on an edge property key",
			err:        txn.CheckSchemaField("edge property key", string(make([]byte, 65536))),
			want:       "Neo.ClientError.Schema.TokenLengthError",
			clientSide: true,
		},
		{
			name:       "value: property value over the fold cap",
			err:        fmt.Errorf("cypher: commit WAL: %w", fmt.Errorf("%w: property value is 1073741825 bytes, maximum 1073741824", txn.ErrValueTooLong)),
			want:       "Neo.ClientError.Data.DataUnsupportedByStoreFormat",
			clientSide: true,
		},
		{
			name:       "neither: per-edge-handle count (the umbrella alone)",
			err:        fmt.Errorf("%w: edge handle property count is 1048577 bytes, maximum 1048576", txn.ErrFieldTooLong),
			want:       "Neo.ClientError.Data.DataUnsupportedByStoreFormat",
			clientSide: true,
		},
		{
			name:       "argument error keeps its own code",
			err:        fmt.Errorf("cypher: ArgumentError.NumberOutOfRange: substring start -1 is negative"),
			want:       "Neo.ClientError.Statement.ArgumentError",
			clientSide: true,
		},
		{
			name:       "snapshot.ErrFieldTooLong is a masked server fault (#2827)",
			err:        fmt.Errorf("checkpoint: capture: %w", fmt.Errorf("%w: label name is 2097152 bytes, maximum 1048576", snapshot.ErrFieldTooLong)),
			want:       "Neo.DatabaseError.General.UnknownError",
			clientSide: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.err == nil {
				t.Fatal("the case built a nil error")
			}
			if got := FailureCode(tc.err); got != tc.want {
				t.Errorf("FailureCode = %q, want %q (err %q)", got, tc.want, tc.err)
			}
			if got := isClientFaultErr(tc.err); got != tc.clientSide {
				t.Errorf("isClientFaultErr = %v, want %v", got, tc.clientSide)
			}
		})
	}
}

// TestFieldKindSentinels_MatchTheUmbrella_2942 pins the compatibility promise of
// the split: every refusal store/txn raises still matches txn.ErrFieldTooLong,
// and the kinds do not match each other.
//
// Since rmp #2748, txn.ErrTokenTooLong IS lpg.ErrTokenTooLong, so a token
// refusal from the in-memory engine matches it too. That bare lpg sentinel does
// NOT match the txn umbrella — graph/lpg knows nothing of the WAL — which is why
// the token row below takes a refusal raised by store/txn itself.
func TestFieldKindSentinels_MatchTheUmbrella_2942(t *testing.T) {
	t.Parallel()
	tok := txn.CheckSchemaField("node label", string(make([]byte, lpg.MaxTokenLen+1)))
	val := fmt.Errorf("%w: x", txn.ErrValueTooLong)
	lpgTok := lpg.CheckToken("node label", string(make([]byte, lpg.MaxTokenLen+1)))
	for name, c := range map[string]struct {
		err    error
		target error
		want   bool
	}{
		"txn token is umbrella":      {tok, txn.ErrFieldTooLong, true},
		"txn token is lpg token":     {tok, lpg.ErrTokenTooLong, true},
		"lpg token is txn token":     {lpgTok, txn.ErrTokenTooLong, true},
		"lpg token is not umbrella":  {lpgTok, txn.ErrFieldTooLong, false},
		"value is umbrella":          {val, txn.ErrFieldTooLong, true},
		"token is not value":         {tok, txn.ErrValueTooLong, false},
		"value is not token":         {val, txn.ErrTokenTooLong, false},
		"umbrella not token":         {txn.ErrFieldTooLong, txn.ErrTokenTooLong, false},
		"sentinels are the same one": {txn.ErrTokenTooLong, lpg.ErrTokenTooLong, true},
	} {
		if got := errors.Is(c.err, c.target); got != c.want {
			t.Errorf("%s: errors.Is = %v, want %v", name, got, c.want)
		}
	}
}
