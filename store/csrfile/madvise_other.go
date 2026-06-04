//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package csrfile

// setHint is a no-op on platforms without madvise. The caller
// ([Reader.SetHint]) holds the read lock and has already verified the
// mapping is live and rejected a closed Reader.
func (r *Reader) setHint(_ AccessPattern) error {
	return nil
}
