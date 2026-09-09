package snapshot

// mapper_verify_decodable_test.go — the drift guard for rmp #2780.
//
// The checkpointer refuses to discard the WAL prefix behind a published
// snapshot whose mapper keys the codec cannot decode, and it decides that with
// [VerifyMapperDecodable]. Recovery decides the same question with the decode
// step of [ApplyMapperToGraphWithCodec]. If those two ever disagreed in the
// permissive direction, the checkpointer would release the only other copy of
// the data behind an image recovery then refuses — the exact Durability defect
// rmp #2749 and rmp #2780 close between them.
//
// They cannot drift today because both go through [decodeMapperKey]. This test
// is what makes that a checked property rather than a comment: it drives both
// entry points over the same readbacks and requires the SAME verdict and, for
// every rejection, the SAME error text and the same [errors.Is] classification.

import (
	"bytes"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// validCodecReadback returns a genuine version-2 mapper readback, produced by
// running the real writer over a real mapper and parsing the bytes back with
// the real reader. Poisoned cases below mutate a copy of it, so every case
// starts from an image the writer actually emits.
func validCodecReadback(t *testing.T) MapperReadback {
	t.Helper()
	m := graph.NewMapper[int]()
	for _, k := range []int{1, 2, 3, 7, 11} {
		m.Intern(k)
	}
	var buf bytes.Buffer
	if _, _, err := WriteMapper[int](&buf, m, txn.NewIntCodec(), nil); err != nil {
		t.Fatalf("WriteMapper[int]: %v", err)
	}
	rb, err := ReadMapperBytes(&buf)
	if err != nil {
		t.Fatalf("ReadMapperBytes: %v", err)
	}
	if len(rb.RawPairs) == 0 {
		t.Fatal("writer produced no version-2 records: the fixture would prove nothing")
	}
	return rb
}

// cloneReadback deep-copies rb so a mutation in one case cannot leak into
// another, and so the two entry points under comparison are each handed their
// own bytes.
func cloneReadback(rb MapperReadback) MapperReadback {
	out := MapperReadback{RawPairs: make([]MapperRawPair, len(rb.RawPairs))}
	for i := range rb.RawPairs {
		out.RawPairs[i] = MapperRawPair{
			ID:  rb.RawPairs[i].ID,
			Key: append([]byte(nil), rb.RawPairs[i].Key...),
		}
	}
	return out
}

func TestVerifyMapperDecodable_AgreesWithRecoverysDecode(t *testing.T) {
	t.Parallel()
	base := validCodecReadback(t)

	cases := []struct {
		name string
		// mutate damages a clone of the valid readback; nil leaves it intact.
		mutate func(MapperReadback) MapperReadback
		// nilCodec drives both entry points with no codec at all.
		nilCodec bool
		wantErr  bool
		// wantIs, when non-nil, must be reported by errors.Is on both errors.
		wantIs []error
	}{
		{
			name:    "a valid version-2 mapper is accepted",
			wantErr: false,
		},
		{
			name: "an undecodable key encoding is refused",
			mutate: func(rb MapperReadback) MapperReadback {
				// A lone varint continuation byte: complete framing, no decodable
				// value. binary.Varint reports n == 0 and the int codec refuses it.
				rb.RawPairs[0].Key = []byte{0x80}
				return rb
			},
			wantErr: true,
			wantIs:  []error{ErrMapperApply, txn.ErrCodecDecode},
		},
		{
			name: "bytes left after the key are refused",
			mutate: func(rb MapperReadback) MapperReadback {
				rb.RawPairs[1].Key = append(rb.RawPairs[1].Key, 0x00)
				return rb
			},
			wantErr: true,
			wantIs:  []error{ErrMapperApply},
		},
		{
			name: "an empty readback is accepted by both",
			mutate: func(MapperReadback) MapperReadback {
				return MapperReadback{}
			},
			wantErr: false,
		},
		{
			name:     "a nil codec with records to decode is refused",
			nilCodec: true,
			wantErr:  true,
			wantIs:   []error{ErrMapperApply},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			verifyRB := cloneReadback(base)
			applyRB := cloneReadback(base)
			if tc.mutate != nil {
				verifyRB = tc.mutate(verifyRB)
				applyRB = tc.mutate(applyRB)
			}

			var verifyErr, applyErr error
			g := lpg.New[int, int64](adjlist.Config{Directed: true})
			if tc.nilCodec {
				verifyErr = VerifyMapperDecodable[int](verifyRB, nil)
				applyErr = ApplyMapperToGraphWithCodec[int, int64](g, applyRB, nil)
			} else {
				verifyErr = VerifyMapperDecodable[int](verifyRB, txn.NewIntCodec())
				applyErr = ApplyMapperToGraphWithCodec[int, int64](g, applyRB, txn.NewIntCodec())
			}

			// THE AGREEMENT: the checkpointer's verdict and recovery's verdict
			// must be the same verdict. A permissive disagreement here is a
			// Durability defect, not a cosmetic one.
			if (verifyErr != nil) != (applyErr != nil) {
				t.Fatalf("verdicts disagree: VerifyMapperDecodable=%v, "+
					"ApplyMapperToGraphWithCodec=%v — the checkpointer and recovery would "+
					"reach different conclusions about the same mapper", verifyErr, applyErr)
			}
			if (verifyErr != nil) != tc.wantErr {
				t.Fatalf("VerifyMapperDecodable error = %v, want error: %v", verifyErr, tc.wantErr)
			}
			if !tc.wantErr {
				return
			}
			// Same rejection, stated the same way. The messages are what an
			// operator reads out of the checkpoint's LastError, and the sentinels
			// are what callers branch on.
			if verifyErr.Error() != applyErr.Error() {
				t.Errorf("rejection text drifted:\n  verify: %s\n  apply:  %s",
					verifyErr.Error(), applyErr.Error())
			}
			for _, target := range tc.wantIs {
				if !errors.Is(verifyErr, target) {
					t.Errorf("errors.Is(verifyErr, %v) = false; err = %v", target, verifyErr)
				}
				if !errors.Is(applyErr, target) {
					t.Errorf("errors.Is(applyErr, %v) = false; err = %v", target, applyErr)
				}
			}
		})
	}
}

// TestVerifyMapperDecodable_IgnoresTheVersion1StringLayout pins the no-op case
// the production readback relies on: a string-keyed store publishes the frozen
// version-1 mapper, whose keys land in [MapperReadback.Pairs] and carry no
// codec framing at all. Recovery decodes nothing for them
// ([ApplyMapperToGraph], no codec), so neither may the checkpointer — and it
// must not refuse the image for having no RawPairs either.
func TestVerifyMapperDecodable_IgnoresTheVersion1StringLayout(t *testing.T) {
	t.Parallel()
	m := graph.NewMapper[string]()
	for _, k := range []string{"alice", "bob", "carol"} {
		m.Intern(k)
	}
	var buf bytes.Buffer
	if _, _, err := WriteMapperString(&buf, m, nil); err != nil {
		t.Fatalf("WriteMapperString: %v", err)
	}
	rb, err := ReadMapperString(&buf)
	if err != nil {
		t.Fatalf("ReadMapperString: %v", err)
	}
	if len(rb.Pairs) == 0 {
		t.Fatal("version-1 readback carries no Pairs: the fixture proves nothing")
	}
	if len(rb.RawPairs) != 0 {
		t.Fatalf("version-1 readback carries %d RawPairs, want 0", len(rb.RawPairs))
	}
	if err := VerifyMapperDecodable[string](rb, txn.NewStringCodec()); err != nil {
		t.Errorf("VerifyMapperDecodable refused a version-1 string mapper: %v — the "+
			"checkpointer would abort every string-keyed checkpoint", err)
	}
	// And with no codec in hand at all, which is how a string-keyed
	// checkpointer configured without WithMapperCodec reaches it.
	if err := VerifyMapperDecodable[string](rb, nil); err != nil {
		t.Errorf("VerifyMapperDecodable refused a version-1 string mapper with a nil "+
			"codec: %v — there is nothing to decode, so there is nothing to refuse", err)
	}
}
