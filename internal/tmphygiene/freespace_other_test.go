//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package tmphygiene

// freespace_other_test.go — the non-Unix fallback for the statfs(2) binding in
// freespace_unix_test.go.
//
// It reports the figure as unobservable rather than guessing a number. The gate
// then degrades to an explicit environment-precondition skip that names the
// missing capability, which is honest; a fabricated "plenty of room" would be a
// gate that silently cannot fail.

import "errors"

// errFreeSpaceUnsupported is returned on platforms with no statfs binding here.
var errFreeSpaceUnsupported = errors.New("no statfs binding for this platform")

// availableBytes reports that free space cannot be observed on this platform.
func availableBytes(_ string) (uint64, error) {
	return 0, errFreeSpaceUnsupported
}
