//go:build linux || darwin || freebsd || netbsd || openbsd

package wal

import "syscall"

// walNoFollow is OR-ed into the open flags of every WAL file the writer
// creates or reopens — the data file, the suffix temp used by prefix
// truncation, and the LOCK sentinel. O_NOFOLLOW makes the open fail with
// ELOOP if the FINAL path component is a symlink, rather than dereferencing
// it.
//
// This closes a symlink-escape (CWE-59) that mirrors the threat the snapshot
// layer already defends via [github.com/FlavioCFOliveira/GoGraph/store/snapshot]'s
// openSnapshotComponent: a store directory adopted from an untrusted source
// (a tampered backup, a shared filesystem) whose "wal" or "wal.tmp" entry is a
// symlink to a process-writable victim file would otherwise let the writer
// append the mutation stream to — or, via the O_TRUNC suffix temp, truncate
// and overwrite — that arbitrary file.
//
// O_NOFOLLOW guards only the final component, which is exactly the
// component-is-a-symlink threat: the WAL path is a fixed name under the
// caller-supplied store directory. It is behaviour-preserving for the regular
// files the writer itself creates (a create/open/append of a real file is
// unaffected); it only rejects a symlinked final component.
const walNoFollow = syscall.O_NOFOLLOW
