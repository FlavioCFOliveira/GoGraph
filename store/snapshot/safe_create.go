package snapshot

import "os"

// snapshotFileMode is the permission mode for every snapshot component
// file. Snapshot directories are already created 0o750, but os.Create
// opens files 0o666 (subject to umask), so a default umask leaves them
// world- or group-readable. Snapshot payloads can carry sensitive graph
// data, so the files themselves are tightened to owner-only read/write.
const snapshotFileMode os.FileMode = 0o600

// createSnapshotFile creates (or truncates) a snapshot component file for
// writing with owner-only permissions (0o600), replacing os.Create, which
// would create it 0o666 & ~umask. The flags match os.Create's
// create/truncate semantics minus the read access the writers never use,
// so the existing atomic tmp -> rename publish protocol is unchanged: the
// caller still writes into a staging .tmp directory and renames it into
// place.
func createSnapshotFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, snapshotFileMode) //nolint:gosec // caller-controlled staging directory under the snapshot dir
}
