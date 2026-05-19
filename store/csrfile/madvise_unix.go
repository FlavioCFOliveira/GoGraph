//go:build linux || darwin || freebsd || netbsd || openbsd

package csrfile

import (
	"golang.org/x/sys/unix"
)

func (r *Reader) setHint(pattern AccessPattern) error {
	if err := r.ensureOpen(); err != nil {
		return err
	}
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
