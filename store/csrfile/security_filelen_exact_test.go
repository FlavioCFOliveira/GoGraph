package csrfile

// Security test battery — csrfile exact-size structural gate.
//
// DEFENSE LOCK-INS (pass today). The mmap reader's safety rests on
// Header.validate proving every typed section lies wholly within the mapped
// region before bindSlices reinterprets it with unsafe.Slice. header_security_
// test.go drives hostile HEADER fields against a constant-length file; this
// file pins the orthogonal axis the reader equally depends on — the file
// LENGTH itself:
//
//   - A genuine, self-consistent header whose backing file has been truncated
//     or extended by even one byte is rejected with ErrHeaderInconsistent
//     (which wraps ErrFileCorrupted), because validate requires
//     total == fileLen exactly, never an inequality. A short file would
//     otherwise let bindSlices read past the mapping; a long file signals a
//     layout the writer never produced.
//   - The same exactness is pinned at the Header.validate unit level, off the
//     mmap path, so the contract is independent of Open.
//
// All cases reseal a correct tail CRC over the mutated image (when the tail is
// still in range) so the CRC gate cannot be what rejects the file — proving
// validate does the rejecting, before any unsafe slice.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSec_Store_OpenRejectsWrongFileLength writes a valid csrfile, then
// appends or truncates bytes so the on-disk length no longer matches the
// canonical layout the header describes. Open must reject it with
// ErrFileCorrupted and must never panic on an out-of-bounds slice.
func TestSec_Store_OpenRejectsWrongFileLength(t *testing.T) {
	t.Parallel()

	base := writeValidCSR(t)
	good, err := DecodeHeader(base)
	if err != nil {
		t.Fatalf("DecodeHeader(base): %v", err)
	}

	cases := []struct {
		name  string
		mutle func(orig []byte) []byte
	}{
		{
			name: "one-byte-longer",
			mutle: func(orig []byte) []byte {
				out := make([]byte, len(orig)+1)
				copy(out, orig)
				return out // trailing zero byte; header still describes the shorter layout
			},
		},
		{
			name: "64-bytes-longer",
			mutle: func(orig []byte) []byte {
				out := make([]byte, len(orig)+int(Alignment))
				copy(out, orig)
				return out
			},
		},
		{
			name: "one-byte-shorter",
			mutle: func(orig []byte) []byte {
				// Drop the final byte: the file is now shorter than the
				// header's TailCRCOffset+4. The tail CRC slice would be
				// out of range; validate must reject before Open reaches it.
				out := make([]byte, len(orig)-1)
				copy(out, orig[:len(orig)-1])
				return out
			},
		},
		{
			name: "truncated-into-edges",
			mutle: func(orig []byte) []byte {
				// Truncate to just past the vertices section so the edges
				// section is incomplete. The header still claims the full
				// edge count, so total != fileLen.
				cut := int(good.EdgesOffset) + 8
				if cut >= len(orig) {
					cut = len(orig) / 2
				}
				out := make([]byte, cut)
				copy(out, orig[:cut])
				return out
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data := tc.mutle(base)
			// Reseal a correct CRC over the mutated image when the header's
			// declared tail offset is still addressable, so the CRC gate is
			// not what rejects the file. When the file is now too short to
			// hold the tail, resealCRC is a no-op and validate rejects on the
			// size mismatch first regardless.
			resealCRC(data, good.TailCRCOffset)

			path := filepath.Join(t.TempDir(), "wronglen.csr")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			r, openErr := openNoPanic(t, path)
			if openErr == nil {
				if r != nil {
					_ = r.Close()
				}
				t.Fatalf("Open accepted a file whose length disagrees with its header (%s); want ErrFileCorrupted", tc.name)
			}
			if !errors.Is(openErr, ErrFileCorrupted) {
				t.Fatalf("Open(%s) = %v; want errors.Is(ErrFileCorrupted)", tc.name, openErr)
			}
		})
	}
}

// TestSec_Store_HeaderValidateExactSize pins the exact-size contract at the
// Header.validate unit level, off the mmap path. A genuine header validates
// only against the precise fileLen its layout requires; any other length —
// one byte over or under, or a wildly wrong value — is ErrFileCorrupted.
func TestSec_Store_HeaderValidateExactSize(t *testing.T) {
	t.Parallel()

	base := writeValidCSR(t)
	good, err := DecodeHeader(base)
	if err != nil {
		t.Fatalf("DecodeHeader(base): %v", err)
	}
	// The canonical total for this header equals the real file length.
	canonical := len(base)
	if err := good.validate(canonical); err != nil {
		t.Fatalf("validate(canonical length %d) = %v, want nil", canonical, err)
	}

	for _, bad := range []int{
		canonical - 1,
		canonical + 1,
		canonical + int(Alignment),
		0,
		1,
	} {
		bad := bad
		// itoa is the existing small int->string helper in this package's
		// test set (writer_enospc_test.go); reused here to label subtests
		// without pulling strconv into the import list.
		t.Run("len="+itoa(bad), func(t *testing.T) {
			t.Parallel()
			if err := good.validate(bad); !errors.Is(err, ErrFileCorrupted) {
				t.Fatalf("validate(%d) = %v, want errors.Is(ErrFileCorrupted)", bad, err)
			}
		})
	}
}
