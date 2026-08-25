package csrfile

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// symlinkEscapeCSR builds a small CSR to publish.
func symlinkEscapeCSR(t *testing.T) *csr.CSR[struct{}] {
	t.Helper()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < 8; i++ {
		if err := a.AddEdge(i, (i+1)%8, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	return csr.BuildFromAdjList(a)
}

// requireSymlinks skips when the platform cannot create one, which is the
// documented scope of the guard: O_NOFOLLOW has no meaning where symlinks do
// not exist, and csrNoFollow is zero there by construction.
func requireSymlinks(t *testing.T, dir string) {
	t.Helper()
	probe := filepath.Join(dir, ".symlink-probe")
	if err := os.Symlink(filepath.Join(dir, ".symlink-target"), probe); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	_ = os.Remove(probe)
}

// TestCSRFile_PublishRefusesASymlinkedTemp is the write half of rmp #2580
// (CWE-59, link following).
//
// The temp name is fully predictable — the writer forms it as
// OutputPath + ".tmp" — so a local principal who can write the store directory
// can pre-plant a symlink there aimed at any file this process may write.
// Without O_NOFOLLOW the O_TRUNC create followed the link and the victim was
// truncated and overwritten with CSR bytes; the publish rename(2) then moved the
// SYMLINK onto the output name, so the published target itself became a link to
// the victim.
//
// The assertion is the VICTIM's bytes, not merely that an error was returned: a
// publish that failed AFTER clobbering the file would satisfy an error-only
// check while doing the exact damage this guards against.
func TestCSRFile_PublishRefusesASymlinkedTemp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	requireSymlinks(t, dir)

	victim := filepath.Join(dir, "victim.txt")
	const victimContent = "a file the process may write, but must not be made to write\n"
	if err := os.WriteFile(victim, []byte(victimContent), 0o600); err != nil {
		t.Fatalf("seed victim: %v", err)
	}

	out := filepath.Join(dir, "graph.csr")
	if err := os.Symlink(victim, out+".tmp"); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	_, err := WriteToFile(out, symlinkEscapeCSR(t))
	if err == nil {
		t.Errorf("the publish SUCCEEDED through a symlinked temp; want it refused (rmp #2580)")
	} else if !errors.Is(err, syscall.ELOOP) {
		// Not fatal: what matters is the victim below. But the guard should be
		// the one that fired, not some later failure.
		t.Logf("publish refused with %v (want an ELOOP from O_NOFOLLOW)", err)
	}

	got, rerr := os.ReadFile(victim) //nolint:gosec // test fixture under t.TempDir
	if rerr != nil {
		t.Fatalf("the victim file is unreadable after the publish attempt: %v", rerr)
	}
	if string(got) != victimContent {
		t.Errorf("the victim file was modified through the symlink: got %d byte(s), want the "+
			"original %d. A predictable temp name plus a symlink is an arbitrary-file "+
			"overwrite (CWE-59, rmp #2580)", len(got), len(victimContent))
	}

	// And the published name must not have become a link to the victim.
	if fi, lerr := os.Lstat(out); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("the published path %s is a SYMLINK: rename(2) moved the planted link onto "+
			"the output name, so every later write through it lands on the victim", out)
	}
}

// TestCSRFile_OpenRefusesASymlink is the read half: a csrfile adopted from an
// untrusted source whose entry is a symlink pointing outside the directory must
// not be dereferenced and mmapped.
//
// It is paired with a positive control below, because a reader that refused
// EVERYTHING would pass this on its own.
func TestCSRFile_OpenRefusesASymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	requireSymlinks(t, dir)

	real := filepath.Join(dir, "real.csr")
	if _, err := WriteToFile(real, symlinkEscapeCSR(t)); err != nil {
		t.Fatalf("publish the real file: %v", err)
	}
	link := filepath.Join(dir, "link.csr")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	r, err := Open(link)
	if err == nil {
		_ = r.Close()
		t.Errorf("Open followed a symlink; want it refused (CWE-59, rmp #2580)")
	} else if !errors.Is(err, syscall.ELOOP) {
		t.Logf("Open refused with %v (want an ELOOP from O_NOFOLLOW)", err)
	}
}

// TestCSRFile_PublishAndOpenStillWorkForRegularFiles is the positive control
// the two negatives need. O_NOFOLLOW guards only the FINAL component and must be
// behaviour-preserving for the ordinary files the writer itself creates; without
// this, a guard that refused every open would satisfy both tests above while
// breaking csrfile outright.
func TestCSRFile_PublishAndOpenStillWorkForRegularFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	out := filepath.Join(dir, "plain.csr")

	hdr, err := WriteToFile(out, symlinkEscapeCSR(t))
	if err != nil {
		t.Fatalf("publishing an ordinary file must still work: %v", err)
	}
	if hdr.NEdges == 0 {
		t.Errorf("published header reports %d edges, want the 8 the fixture wrote", hdr.NEdges)
	}

	r, err := Open(out)
	if err != nil {
		t.Fatalf("opening an ordinary file must still work: %v", err)
	}
	defer func() { _ = r.Close() }()

	// A publish through a SYMLINKED PARENT directory must also still work:
	// O_NOFOLLOW guards the final component only, and csrfile paths introduce no
	// attacker-controlled intermediate component. Asserting it pins the scope of
	// the guard rather than leaving it to the comment.
	requireSymlinks(t, dir)
	inner := filepath.Join(dir, "inner")
	if err := os.Mkdir(inner, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	linkedDir := filepath.Join(dir, "linked")
	if err := os.Symlink(inner, linkedDir); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}
	if _, err := WriteToFile(filepath.Join(linkedDir, "through.csr"), symlinkEscapeCSR(t)); err != nil {
		t.Errorf("publishing through a symlinked PARENT directory failed: %v. O_NOFOLLOW must "+
			"guard the final component only", err)
	}
}
