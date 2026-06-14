package snapshot

// Security test battery — end-to-end hostile-count rejection guard (soak layer).
//
// DEFENSE LOCK-IN #1467 (end-to-end). security_decoder_count_bound_test.go pins
// the in-process invariant at a count one past each decoder's ceiling. This
// file closes the loop at a count UNDER the ceiling but far beyond any plausible
// file — one that the OLD reader would have turned into a multi-terabyte eager
// make([]T, count) — by running the load in a CHILD process capped with
// GOMEMLIMIT, so any regression to the unbounded reservation can never threaten
// the parent test process.
//
// Now that ReadLabels clamps its eager reservation (capHint(count,
// labelsCapHintMax); see labels.go), a hostile nodeCount of 1<<40 reserves only
// labelsCapHintMax (1<<20) entries — ~16 MiB, comfortably under the child's
// 256 MiB GOMEMLIMIT — and the first per-record read then hits EOF, so
// LoadSnapshotFull returns ErrCorrupted. The child therefore REJECTS the
// snapshot cleanly and exits 0 (printing REJECTED). The guard asserts exactly
// that:
//
//   - REJECTED (exit 0): the loader returned a corruption error without an
//     allocation proportional to the hostile count. This is the post-fix
//     contract — the only accepted outcome.
//   - LOADED (exit 3): LoadSnapshotFull returned nil — the forbidden "silently
//     allocated and parsed" outcome. Fails the test.
//   - OOM-abort or any other non-zero exit: a REGRESSION. Before the clamp this
//     was tolerated (a large make under a tight GOMEMLIMIT could abort the child
//     before the EOF read), but the clamp removes the giant reservation
//     entirely, so an OOM now means the bound was lost. The guard fails on it.
//
// Runs in the soak layer: spawning a child under a tight GOMEMLIMIT is heavier
// and less deterministic than a unit test, and the GOMEMLIMIT inheritance
// perturbs the whole child process.

import (
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/subproc"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

const (
	// secStoreOOMMode is the subproc mode that builds a malicious snapshot
	// declaring a terabyte-scale label-record count and tries to load it.
	secStoreOOMMode = "sec_store_hostile_snapshot_oom"
	// secStoreOOMLoaded is printed by the child only if LoadSnapshotFull
	// returned WITHOUT error — the forbidden "silently allocated and parsed"
	// outcome. Its presence fails the test.
	secStoreOOMLoaded = "SEC_STORE_LOADED"
	// secStoreOOMRejected is printed by the child when LoadSnapshotFull
	// returned a (corruption) error before exhausting memory — the
	// post-fix bounded-rejection outcome.
	secStoreOOMRejected = "SEC_STORE_REJECTED"
	// secStoreOOMHostileRecords is the declared node-label record count in
	// the malicious labels.bin: 1<<40 entries. It sits exactly at the decoder's
	// implausibility ceiling (the guard is `> 1<<40`), so it slips past that
	// ceiling and reaches the eager-reservation site — exercising the capHint
	// clamp on a count UNDER the ceiling. The OLD reader would have reserved
	// 1<<40 * 16 B = 16 TiB here; the clamped reader reserves only
	// labelsCapHintMax entries and then fails the first record read on the
	// truncated body.
	secStoreOOMHostileRecords = uint64(1) << 40
)

func init() {
	subproc.Register(secStoreOOMMode, secStoreHostileSnapshotChild)
}

// secStoreHostileSnapshotChild is the child-process body. It lays out a
// snapshot directory whose labels.bin declares secStoreOOMHostileRecords
// node-label records with a truncated body and whose manifest CRC matches the
// real (tiny) file bytes, then calls LoadSnapshotFull. The child's working
// directory is the parent-provided t.TempDir() (see subproc.RunCtx), so it
// writes the snapshot there.
//
// Exit codes:
//
//	0 — LoadSnapshotFull returned a corruption error (bounded rejection; prints
//	    secStoreOOMRejected). This is the post-fix contract, the only accepted
//	    outcome.
//	3 — LoadSnapshotFull returned nil error (forbidden; prints
//	    secStoreOOMLoaded). The parent fails the test on this.
//
// With the capHint clamp in place the child never OOMs — the hostile count no
// longer drives a giant make(). Should the clamp regress and the giant
// reservation return, the Go runtime aborts the child with a non-zero exit
// before either line is printed; the parent now treats that as a failure (the
// bound was lost), not an accepted process-confined outcome.
func secStoreHostileSnapshotChild(_ []string) int {
	dir, err := os.MkdirTemp("", "sec-store-oom-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "child: MkdirTemp: %v\n", err)
		return 1
	}

	// labels.bin: magic + version + empty string table + hostile nodeCount,
	// then a truncated body (no records). A correctly-bounded reader rejects
	// nodeCount against the file's byte budget; the current reader reserves
	// make([]NodeLabelEntry, nodeCount) first.
	var labels []byte
	labels = secStorePutU32(labels, labelsMagic)
	labels = secStorePutU32(labels, labelsFormatVersion)
	labels = secStorePutU64(labels, 0)                         // empty string table
	labels = secStorePutU64(labels, secStoreOOMHostileRecords) // hostile node-label count
	if werr := secStoreWriteComponent(dir, LabelsFile, labels); werr != nil {
		fmt.Fprintf(os.Stderr, "child: write labels.bin: %v\n", werr)
		return 1
	}

	// A minimal valid csr.bin so the loader reaches the labels component.
	// 0 vertices, 0 edges, no weights — an empty but structurally valid CSR.
	var csr []byte
	csr = secStorePutU64(csr, 0) // nV
	csr = secStorePutU64(csr, 0) // nE
	csr = append(csr, 0, 0)      // hasWeights = 0, weightSize = 0
	if werr := secStoreWriteComponent(dir, CSRFile, csr); werr != nil {
		fmt.Fprintf(os.Stderr, "child: write csr.bin: %v\n", werr)
		return 1
	}

	// Manifest referencing both components with truthful sizes and CRCs, so
	// the only thing between the loader and the giant make() is the missing
	// size bound on the labels reader — not the CRC gate (the CRC matches).
	if werr := secStoreWriteManifest(dir, csr, labels); werr != nil {
		fmt.Fprintf(os.Stderr, "child: write manifest: %v\n", werr)
		return 1
	}

	_, loadErr := LoadSnapshotFull(dir)
	if loadErr == nil {
		fmt.Println(secStoreOOMLoaded)
		return 3
	}
	fmt.Println(secStoreOOMRejected)
	return 0
}

// secStoreWriteComponent writes a snapshot component file with owner-only
// permissions, mirroring the writer's posture.
func secStoreWriteComponent(dir, name string, data []byte) error {
	return os.WriteFile(filepath.Join(dir, name), data, 0o600)
}

// secStoreWriteManifest writes a manifest.json referencing csr.bin and
// labels.bin with truthful sizes and CRC32Cs over the supplied bytes.
func secStoreWriteManifest(dir string, csr, labels []byte) error {
	m := Manifest{
		Version:   manifestVersionV2,
		CreatedAt: time.Now().UTC(),
		Files: []FileEntry{
			{Name: CSRFile, Size: int64(len(csr)), CRC32C: secStoreCRC(csr)},
			{Name: LabelsFile, Size: int64(len(labels)), CRC32C: secStoreCRC(labels)},
		},
	}
	f, err := os.OpenFile(filepath.Join(dir, "manifest.json"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if werr := WriteManifest(f, m); werr != nil {
		_ = f.Close()
		return werr
	}
	return f.Close()
}

// secStoreCRC is the package CRC32C (Castagnoli) over b, matching every
// component writer (castagnoli is the package-level table).
func secStoreCRC(b []byte) uint32 {
	return crc32.Checksum(b, castagnoli)
}

// TestSec_Store_HostileSnapshotOOMGuard runs the hostile-count load in a child
// process capped at GOMEMLIMIT=256MiB and asserts the child REJECTS the
// snapshot cleanly: LoadSnapshotFull returns a corruption error (REJECTED, exit
// 0) without a multi-GB allocation. The capHint clamp removes the giant eager
// reservation, so neither a silent load (LOADED) nor an OOM-abort is an
// acceptable outcome any more — both fail the test.
func TestSec_Store_HostileSnapshotOOMGuard(t *testing.T) {
	testlayers.RequireSoak(t)
	// t.Setenv forbids t.Parallel; the child inherits GOMEMLIMIT through the
	// parent's os.Environ() (subproc.RunCtx appends only the mode var).
	t.Setenv("GOMEMLIMIT", "256MiB")
	// A hard cap so a wedged child cannot hang the suite. 90s is generous for
	// the bounded rejection (the only expected outcome).
	stdout, stderr, err := subproc.RunWithTimeout(t, 90*time.Second, secStoreOOMMode)
	out := string(stdout)

	if strings.Contains(out, secStoreOOMLoaded) {
		t.Fatalf("child silently loaded a snapshot declaring %d label records; "+
			"the hostile count was not rejected.\nstdout:\n%s\nstderr:\n%s",
			secStoreOOMHostileRecords, out, stderr)
	}

	switch {
	case err == nil && strings.Contains(out, secStoreOOMRejected):
		// Post-fix bounded rejection: the clamped reader rejected the hostile
		// count without a giant make(). The only accepted outcome.
		t.Logf("child rejected the hostile count cleanly (bounded-rejection path)")
	case err != nil:
		// Regression: the child terminated non-zero. With the clamp there is no
		// giant reservation to exhaust GOMEMLIMIT, so an OOM-abort (or any other
		// non-zero exit) means the bound was lost. Fail loudly.
		var exitErr interface{ ExitCode() int }
		if errors.As(err, &exitErr) {
			t.Fatalf("child terminated non-zero (exit=%d) instead of rejecting cleanly; "+
				"the capHint clamp appears to have regressed (a giant make() returned).\nstdout:\n%s\nstderr:\n%s",
				exitErr.ExitCode(), out, stderr)
		}
		t.Fatalf("child terminated non-zero (%v) instead of rejecting cleanly; "+
			"the capHint clamp appears to have regressed.\nstdout:\n%s\nstderr:\n%s",
			err, out, stderr)
	default:
		t.Fatalf("child exited 0 without the REJECTED marker; unexpected outcome.\nstdout:\n%s\nstderr:\n%s",
			out, stderr)
	}
}
