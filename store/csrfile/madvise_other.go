//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package csrfile

// setHint is a no-op on platforms without madvise.
func (r *Reader) setHint(_ AccessPattern) error {
	return r.ensureOpen()
}
