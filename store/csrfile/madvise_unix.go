//go:build linux || darwin || freebsd || netbsd || openbsd

package csrfile

import (
	"golang.org/x/sys/unix"
)

// setHint issues madvise on the mapped region. The caller
// ([Reader.SetHint]) holds the read lock and has already verified the
// mapping is live, so this method neither locks nor re-checks.
func (r *Reader) setHint(pattern AccessPattern) error {
	advice := 0
	switch pattern {
	case AccessSequential:
		advice = unix.MADV_SEQUENTIAL
	case AccessRandom:
		advice = unix.MADV_RANDOM
	case AccessWillNeed:
		advice = unix.MADV_WILLNEED
	case AccessDontNeed:
		advice = unix.MADV_DONTNEED
	default:
		advice = unix.MADV_NORMAL
	}
	return unix.Madvise(r.mm, advice)
}
