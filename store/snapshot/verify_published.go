package snapshot

import (
	"errors"
	"fmt"
)

// ErrPublishedLegacyShape is returned by [VerifySnapshotReadable] for a
// snapshot whose manifest declares a legacy undirected or simple graph. No
// writer of this build produces one, so a published snapshot that does is not
// the image its writer captured.
var ErrPublishedLegacyShape = errors.New("snapshot: published snapshot declares a legacy graph shape")

// VerifySnapshotReadable establishes that the snapshot published at snapDir
// can be read back and applied by recovery, before anything that depends on
// it — truncating the write-ahead log below it — is done. It returns nil only
// when both halves recovery performs succeed, each with the call recovery
// makes:
//
//   - THE PARSE: [LoadSnapshotFull], which opens and parses every component
//     the manifest declares (rmp #2749).
//   - THE DECODE: [VerifyMapperDecodable] over the mapper readback the parse
//     produced, decoding every codec-encoded key through codec as
//     [ApplyMapperToGraphWithCodec] does next (rmp #2780).
//
// It also refuses, with [ErrPublishedLegacyShape], an image whose manifest
// declares a legacy graph shape.
//
// codec is the store's node-identifier codec. It may be nil, in which case no
// version-2 mapper can have been published and there is nothing to decode. The
// parsed image is discarded on return. It is the one verification both the
// checkpointer and recovery's migration of a legacy store run, and is safe for
// concurrent use with readers of snapDir.
func VerifySnapshotReadable[N comparable](snapDir string, codec keyDecoder[N]) error {
	loaded, err := LoadSnapshotFull(snapDir)
	if err != nil {
		return err
	}
	if loaded.Legacy.Any() {
		return fmt.Errorf("%w: %s", ErrPublishedLegacyShape, snapDir)
	}
	return VerifyMapperDecodable[N](loaded.Mapper, codec)
}

// SelfSufficient reports whether a snapshot whose manifest lists files can
// reconstruct the graph without replaying the write-ahead log below it: it
// carries [MapperFile], and also [ConstraintsFile] when needConstraints is set
// and [IndexDefsFile] when needIndexes is set (the store declares constraints
// or indexes, whose definitions live nowhere else once the log is truncated).
//
// Detection is by manifest content, not version number. It is a composition
// check only: it opens no component, so it does not establish that the image
// parses; [VerifySnapshotReadable] does. Only both together make truncating
// the log below the snapshot safe.
func SelfSufficient(files []FileEntry, needConstraints, needIndexes bool) bool {
	var hasMapper, hasConstraints, hasIndexDefs bool
	for _, f := range files {
		switch f.Name {
		case MapperFile:
			hasMapper = true
		case ConstraintsFile:
			hasConstraints = true
		case IndexDefsFile:
			hasIndexDefs = true
		}
	}
	return hasMapper && (!needConstraints || hasConstraints) && (!needIndexes || hasIndexDefs)
}
