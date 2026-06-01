//go:build linux || darwin || freebsd || netbsd || openbsd

package snapshot

import (
	"os"
	"syscall"
)

// openSnapshotComponent opens a snapshot component file for reading with
// the symlink-following behaviour of the final path component disabled
// (O_NOFOLLOW). A snapshot directory loaded from an untrusted source may
// contain a component file — csr.bin, labels.bin, properties.bin,
// mapper.bin, manifest.json, or an indexes/<name>.bin — that is actually
// a symlink pointing outside the directory (for example at /etc/shadow),
// which would otherwise widen the arbitrary-file-read surface (this
// compounds the manifest path-traversal hardening). With O_NOFOLLOW the
// open fails (ELOOP) rather than dereferencing the link, so the read
// surface is confined to regular files that physically live in the
// snapshot directory.
//
// O_NOFOLLOW guards only the final path component, which is exactly the
// component-file-is-a-symlink threat: every caller passes a path of the
// form filepath.Join(snapshotDir, componentName) where componentName is a
// fixed constant or a name already validated by [validateIndexName], so
// no attacker-controlled intermediate directory component is introduced.
//
// The returned file is owned by the caller, who must Close it. Errors
// (including the symlink rejection) are returned verbatim.
func openSnapshotComponent(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0) //nolint:gosec // path is a fixed component name or validated index name under the snapshot dir
}
