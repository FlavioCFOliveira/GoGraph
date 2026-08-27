//go:build linux || darwin || freebsd || netbsd || openbsd

package csrfile

import "syscall"

// csrNoFollow is OR-ed into the open flags of every csrfile the writer creates
// and every csrfile the reader opens. O_NOFOLLOW makes the open fail with ELOOP
// when the FINAL path component is a symlink, instead of dereferencing it.
//
// It closes a symlink-escape (CWE-59, link following) that the two sibling
// publish paths already defended — store/wal's walNoFollow and store/snapshot's
// openSnapshotComponent, both hardened under rmp #1843 — and that csrfile alone
// was missing (rmp #2580).
//
// The write side was the sharper half, because the temp name is fully
// predictable: the writer forms it as OutputPath + ".tmp". A local principal who
// can write the store directory could pre-plant a symlink there pointing at any
// file the GoGraph process may write; the O_TRUNC create then truncated and
// overwrote the victim, and the publish rename(2) moved the SYMLINK onto the
// output name, so the published target itself became a link to the victim.
//
// O_NOFOLLOW guards only the final component, which is exactly the
// component-is-a-symlink threat: csrfile paths are a caller-supplied output name
// and that name plus a fixed ".tmp" suffix, so no attacker-controlled
// intermediate directory component is introduced. It is behaviour-preserving for
// the regular files the writer itself creates.
const csrNoFollow = syscall.O_NOFOLLOW
