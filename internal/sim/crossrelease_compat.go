package sim

// crossrelease_compat.go — the ON-DISK FORMAT half of cross-release testing
// (rmp #2477).
//
// The cross-release harness next door (crossrelease.go) builds a prior release
// and drives it. That is the strongest evidence available, and it is also the
// slowest and the most environment-dependent: it needs git, a worktree, and a
// tag that still builds with the current toolchain. This file carries the half
// that needs none of them — frozen artefacts in the OLD on-disk shapes, opened
// by the current reader, plus a model of what an OLD reader would have done
// with a NEW artefact.
//
// # Why a model of the old reader, and what it is worth
//
// The forward direction is directly testable: an old artefact is committed, and
// the current reader either opens it or does not. The backward direction — a
// NEW artefact refused by an OLD reader — is not, because the old reader is a
// build that no longer exists in this tree. What CAN be pinned is the DECISION
// RULE that release had, restated here from the two guards it actually
// contained, and applied to a new artefact produced by the current writer.
//
// A model is weaker than the real binary and is treated as such: it is
// two-sided, so it must ACCEPT the frozen old artefact and REFUSE the new one.
// A rule that refused everything would prove nothing, and a rule that accepted
// everything would prove nothing; only a rule that separates the two carries
// information. The prior-tag subprocess in crossrelease_test.go remains the
// authority; this is the part that runs on every change.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// XReleaseFixtureDir is the root of the frozen cross-release on-disk fixtures,
// relative to this package's directory.
const XReleaseFixtureDir = "testdata/xrelease"

// LegacyFullSnapshotDir is the store-root of the frozen PRE-rmp-#2520,
// PRE-rmp-#2526 snapshot fixture: a complete manifest-v3 snapshot directory
// (csr.bin, labels.bin, properties.bin, mapper.bin) whose manifest.json carries
// NO integrity trailer and NO `integrity` key, and whose csr.bin carries the
// dense 8-byte-wide float64 weights column.
//
// It is a store root, not the snapshot directory itself: the snapshot lives at
// LegacyFullSnapshotDir/snapshot, which is where [store/recovery.Open] looks,
// and there is deliberately NO WAL beside it. A recovery over this directory can
// therefore only have obtained the graph from the snapshot bytes.
//
// The bytes are FROZEN. They are pinned by digest in the compatibility test, not
// by a golden-file helper, because a golden can be rewritten by `-update` — and
// a fixture silently regenerated in the CURRENT format would keep every
// assertion passing while testing nothing at all. That is the failure mode rmp
// #2520 avoided by leaving its own frozen fixture unframed.
const LegacyFullSnapshotDir = XReleaseFixtureDir + "/legacy_full_snapshot"

// csrHeaderBytes is the fixed width of csr.bin's header: two uint64 counts then
// the hasWeights and weightSizeBytes flag bytes.
const csrHeaderBytes = 18

// ErrShortCSRHeader is returned by [ReadCSRHeaderBytes] when the input is too
// short to contain a csr.bin header.
var ErrShortCSRHeader = errors.New("sim: cross-release: csr.bin shorter than its header")

// CSRHeaderFacts is a csr.bin header decoded INDEPENDENTLY of the store's own
// reader.
//
// The independence is the point. "The current reader accepts this fixture and
// reports width 8" is one component agreeing with itself; a defect in the header
// parse would move both halves together and stay invisible. A second decoder,
// written from the layout documented on [store/snapshot.WriteCSR], turns that
// into two sources that can disagree.
type CSRHeaderFacts struct {
	// NVertices and NEdges are the two little-endian uint64 counts that open the
	// file. NVertices includes the trailing sentinel offset.
	NVertices uint64
	NEdges    uint64
	// Width is the weightSizeBytes header byte: 1, 2, 4 or 8 for the dense native
	// weights column, 0 when there are no weights, and 0xFF
	// (store/snapshot's weightSizeCodec) for the variable-width codec-encoded
	// section rmp #2526 added.
	Width uint8
	// HasWeights is the hasWeights header byte, decoded as a bool.
	HasWeights bool
}

// SentinelCSRWidth is the weightSizeBytes value rmp #2526 introduced to select
// the variable-width, codec-encoded weights section. It is restated here rather
// than imported because store/snapshot keeps it unexported; the value is part of
// the ON-DISK contract, so a copy in the cross-release model is the same kind of
// restatement the wire protocol next door already makes.
const SentinelCSRWidth uint8 = 0xFF

// ReadCSRHeaderBytes decodes the first [csrHeaderBytes] bytes of a csr.bin.
func ReadCSRHeaderBytes(b []byte) (CSRHeaderFacts, error) {
	if len(b) < csrHeaderBytes {
		return CSRHeaderFacts{}, fmt.Errorf("%w: %d bytes", ErrShortCSRHeader, len(b))
	}
	return CSRHeaderFacts{
		NVertices:  binary.LittleEndian.Uint64(b[0:8]),
		NEdges:     binary.LittleEndian.Uint64(b[8:16]),
		HasWeights: b[16] == 1,
		Width:      b[17],
	}, nil
}

// LegacyCSRVerdict is what a csr.bin reader from BEFORE rmp #2526 would have
// done with a header, and — when it refuses — which of that release's two
// independent guards refused it.
type LegacyCSRVerdict struct {
	// Reason describes the outcome in the terms of the guard that produced it.
	Reason string
	// Accepted reports that neither guard refused.
	Accepted bool
	// WidthGuardRefused reports the ApplyCSRToGraph guard: that build compared
	// the header width against csrWeightSize[W](), which returned only 0, 1, 2, 4
	// or 8, and returned ErrCorrupted on any mismatch.
	WidthGuardRefused bool
	// ExtentGuardRefused reports the readCSRLimited guard: that build computed
	// the dense weights extent as width*nEdges and rejected it against the
	// manifest-recorded file size.
	ExtentGuardRefused bool
}

// LegacyCSRReaderVerdict applies the PRE-rmp-#2526 reader's decision rule to a
// header. nativeWidth is the width that build's csrWeightSize[W]() returned for
// the weight type it was compiled with — 8 for the float64 stores every release
// of this module has shipped, 4 for float32, and so on.
//
// The rule is restated from the two guards that release contained, and both are
// evaluated so a refusal reports whether it was over-determined:
//
//   - The EXTENT guard, in readCSRLimited. The dense layout implies exactly
//     width*nEdges weight bytes, which cannot exceed the file the manifest
//     describes. The rmp #2526 sentinel is 0xFF, so a new artefact claims 255
//     bytes per edge and blows this bound on any file with edges.
//   - The WIDTH guard, in ApplyCSRToGraph. The header width had to equal the
//     compiled-in native width exactly. 0xFF is not a width csrWeightSize could
//     return for any W, so it can never match.
//
// A zero fileSize means "unknown", which disables the extent guard alone — the
// width guard still decides. That mirrors the real reader, whose extent bound is
// only as tight as the manifest size it was given.
func LegacyCSRReaderVerdict(h CSRHeaderFacts, fileSize int64, nativeWidth uint8) LegacyCSRVerdict {
	if !h.HasWeights {
		// A weightless file has no weights section for either guard to judge, and
		// its width byte is 0 by construction.
		return LegacyCSRVerdict{Accepted: true, Reason: "weightless csr.bin: no weights section to adjudicate"}
	}
	v := LegacyCSRVerdict{Accepted: true}
	if fileSize > 0 && uint64(h.Width)*h.NEdges > uint64(fileSize) {
		v.ExtentGuardRefused = true
		v.Accepted = false
		v.Reason = fmt.Sprintf("extent guard: dense weights extent %d*%d = %d bytes exceeds the %d-byte file",
			h.Width, h.NEdges, uint64(h.Width)*h.NEdges, fileSize)
	}
	if h.Width != nativeWidth {
		v.WidthGuardRefused = true
		v.Accepted = false
		if v.Reason != "" {
			v.Reason += "; "
		}
		v.Reason += fmt.Sprintf("width guard: header width %d != compiled-in native width %d", h.Width, nativeWidth)
	}
	if v.Accepted {
		v.Reason = fmt.Sprintf("dense width %d matches the compiled-in native width and fits the file", h.Width)
	}
	return v
}

// ManifestFraming is a manifest.json's rmp #2520 framing, decoded from the raw
// bytes independently of [store/snapshot.LoadManifest].
//
// It answers the byte-level question the loader answers semantically: is the
// integrity trailer physically there, and does the document declare one? The
// loader's own IntegrityVerified is the semantic answer; a fixture assertion
// that used only that would be trusting the component under test to describe its
// own input.
type ManifestFraming struct {
	// DeclaredIntegrity is the value of the document's `integrity` key, empty
	// when the key is absent. rmp #2520's writer always sets it; a manifest from
	// before it never has it.
	DeclaredIntegrity string
	// PayloadEnd is the offset one past the closing brace of the JSON value.
	PayloadEnd int64
	// TrailerBytes is how many bytes follow the JSON value.
	TrailerBytes int
	// TrailerMagicPresent reports that the last 16 bytes open with rmp #2520's
	// trailer magic. The magic starts with a NUL precisely so it can never be
	// mistaken for the JSON whitespace a legacy manifest ends with.
	TrailerMagicPresent bool
}

// manifestTrailerMagic mirrors store/snapshot's unexported trailer magic. Like
// [SentinelCSRWidth], it is part of the on-disk contract rather than of the
// package's Go API, so restating it here is what lets this file adjudicate the
// bytes without the writer's help.
var manifestTrailerMagic = [8]byte{0x00, 'G', 'G', 'M', 'A', 'N', 'I', 'F'}

// manifestTrailerSize mirrors store/snapshot's trailer width: magic, algorithm
// identifier, checksum.
const manifestTrailerSize = 16

// InspectManifestBytes decodes the framing of a manifest.json from its raw
// bytes. A malformed document yields a zero PayloadEnd and no trailer, which the
// caller reads as "not a framed manifest" — the same conclusion the loader draws
// before it refuses.
func InspectManifestBytes(b []byte) ManifestFraming {
	var out ManifestFraming
	var doc struct {
		Integrity string `json:"integrity"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&doc); err != nil {
		return out
	}
	out.DeclaredIntegrity = doc.Integrity
	out.PayloadEnd = dec.InputOffset()
	out.TrailerBytes = len(b) - int(out.PayloadEnd)
	if len(b) >= manifestTrailerSize {
		tail := b[len(b)-manifestTrailerSize:]
		out.TrailerMagicPresent = bytes.Equal(tail[:8], manifestTrailerMagic[:])
	}
	return out
}

// Framed reports that this manifest carries the rmp #2520 integrity trailer:
// bytes beyond the JSON value AND the trailer magic at the very end. Whitespace
// after the closing brace — every manifest ends with the newline the JSON
// encoder appends — is not a trailer, which is exactly the rule
// store/snapshot's splitManifestTrailer applies.
func (f ManifestFraming) Framed() bool {
	return f.TrailerMagicPresent && f.TrailerBytes >= manifestTrailerSize
}

// ReadFixtureFile reads one file out of the frozen fixture tree and returns its
// bytes. The path is joined under this package's directory, so it resolves the
// same way `go test` resolves testdata.
func ReadFixtureFile(rel ...string) ([]byte, error) {
	p := filepath.Join(rel...)
	b, err := os.ReadFile(p) //nolint:gosec // p is a constant, repo-internal fixture path
	if err != nil {
		return nil, fmt.Errorf("sim: cross-release: read fixture %q: %w", p, err)
	}
	return b, nil
}
