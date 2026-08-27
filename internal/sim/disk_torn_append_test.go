package sim

// disk_torn_append_test.go — a truncated trailing frame must be reachable
// (rmp #2541, audit finding F7).
//
// # What was unreachable, and why the durable shadow did not deliver it
//
// A truncated trailing frame is the single most important crash state a
// write-ahead log must survive, and the simulator could not produce one by any
// route. Write returned (0, ENOSPC) and grew nothing, so there was no short
// write; Crash never shortened a file beyond its durable image; corruptSector
// flips a byte and never truncates, which is a CRC failure and a different
// defect class. The harness compensated out of band, truncating the file
// directly — a state the simulator chose rather than one a simulated crash
// produced.
//
// The durable shadow (F1) does NOT deliver this for free, which is the question
// the task asked. CrashHost reverts each file to data[:durableLen], and
// durableLen moves only on a successful Sync — so a crash truncates at the last
// SYNC boundary, which for a WAL is a FRAME boundary. That is a cleanly LOST
// trailing frame. A lost tail and a torn tail exercise different branches of
// recovery: a reader that handles a clean truncation can still mishandle a frame
// whose header declares one length and whose body stops short.
//
// Two routes now exist and both are tested here: the POSIX short-count contract,
// which makes a partial record EMERGENT under a full disk, and ArmTornAppendAt,
// which makes one reachable at a chosen record and any capacity.

import (
	"bytes"
	"errors"
	"os"
	"syscall"
	"testing"
)

// tornOpen opens path for append on d, failing the test on error.
func tornOpen(t *testing.T, d *SimDisk, path string) *SimFileHandle {
	t.Helper()
	h, err := d.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", path, err)
	}
	return h
}

// tornRead returns the whole file at path.
func tornRead(t *testing.T, d *SimDisk, path string) []byte {
	t.Helper()
	h, err := d.OpenFile(path, os.O_RDWR)
	if err != nil {
		t.Fatalf("reopen(%s): %v", path, err)
	}
	defer func() { _ = h.Close() }()
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := h.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil || n == 0 {
			return out
		}
	}
}

// TestSimDisk_ShortWriteOnPartialSpace is the short-count contract: a write that
// only partly fits TRANSFERS WHAT FITS and reports that count together with
// ENOSPC.
//
// The count is what matters — before #2541 a partially-satisfiable write
// transferred NOTHING, so a partial record could not exist and a torn trailing
// frame was unreachable. The error is not optional: SimFileHandle is used as an
// io.Writer, whose contract is that "Write must return a non-nil error if it
// returns n < len(p)", and os.File.Write attaches one to every short count.
// Returning the count with a nil error was tried and a real caller in this tree
// rejected it — see the note in SimFileHandle.Write.
func TestSimDisk_ShortWriteOnPartialSpace(t *testing.T) {
	d := NewSimDisk(NewSeed(1), 0)
	d.SetCapacity(100, false)

	h := tornOpen(t, d, "/wal")
	defer func() { _ = h.Close() }()

	// 60 bytes fit outright.
	if n, err := h.Write(bytes.Repeat([]byte{'a'}, 60)); err != nil || n != 60 {
		t.Fatalf("first write = (%d, %v), want (60, nil)", n, err)
	}

	// 60 more do not: 40 fit. The PARTIAL TRANSFER is the observation this test
	// exists for — before #2541 this returned (0, ENOSPC) and the file stayed at
	// 60 bytes, so no partial record could ever exist.
	n, err := h.Write(bytes.Repeat([]byte{'b'}, 60))
	if n != 40 {
		t.Fatalf("partial write transferred %d bytes, want 40 (capacity 100, 60 already used); "+
			"a write that partly fits must transfer what fits, or no partial record can exist", n)
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("partial write returned %v, want ENOSPC alongside the short count; io.Writer "+
			"requires a non-nil error whenever n < len(p), and os.File.Write attaches one", err)
	}

	// And a write with no room at all is refused outright.
	if n, err := h.Write([]byte{'c'}); n != 0 || !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("the write after the disk filled returned (%d, %v), want (0, ENOSPC)", n, err)
	}

	// The file holds exactly what was transferred, in order.
	got := tornRead(t, d, "/wal")
	want := append(bytes.Repeat([]byte{'a'}, 60), bytes.Repeat([]byte{'b'}, 40)...)
	if !bytes.Equal(got, want) {
		t.Errorf("file is %d bytes, want %d; a short write must transfer a PREFIX of the payload",
			len(got), len(want))
	}
}

// TestSimDisk_TornAppendSurvivesCrash is the arm: a crash leaves the last record
// partial, with its kept prefix intact and its remainder garbled.
func TestSimDisk_TornAppendSurvivesCrash(t *testing.T) {
	const (
		frame = 32
		keep  = 12
	)
	d := NewSimDisk(NewSeed(7), 0)

	h := tornOpen(t, d, "/wal")

	// Two whole frames, synced: these are durable and must survive intact.
	first := bytes.Repeat([]byte{'A'}, frame)
	second := bytes.Repeat([]byte{'B'}, frame)
	for _, f := range [][]byte{first, second} {
		if _, err := h.Write(f); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := h.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Arm the NEXT growing write to tear, then issue the third frame and crash
	// before syncing it.
	d.ArmTornAppendAt(1, keep)
	third := bytes.Repeat([]byte{'C'}, frame)
	if _, err := h.Write(third); err != nil {
		t.Fatalf("write third: %v", err)
	}
	_ = h.Close()
	d.CrashHost()

	got := tornRead(t, d, "/wal")

	// The file is LONGER than the durable image: a torn append is bytes that
	// reached the platter without a completed fsync, so it extends the durable
	// prefix rather than shortening it.
	if len(got) != 2*frame+frame {
		t.Fatalf("file is %d bytes, want %d (two durable frames plus the torn third)",
			len(got), 3*frame)
	}

	// The two synced frames are untouched.
	if !bytes.Equal(got[:frame], first) || !bytes.Equal(got[frame:2*frame], second) {
		t.Fatalf("a torn append damaged an already-durable frame")
	}

	// The torn frame keeps its first `keep` bytes...
	tail := got[2*frame:]
	if !bytes.Equal(tail[:keep], third[:keep]) {
		t.Errorf("the torn frame's first %d bytes are %q, want the real payload %q",
			keep, tail[:keep], third[:keep])
	}

	// ...and the remainder is GARBAGE: not the payload, and not zeroes. Zero-fill
	// is the benign model this deliberately does not use, and a reader that only
	// ever meets zeros can pass while mishandling what a real platter holds.
	rest := tail[keep:]
	if bytes.Equal(rest, third[keep:]) {
		t.Errorf("the discarded remainder still holds the real payload; nothing was torn")
	}
	if allZero(rest) {
		t.Errorf("the discarded remainder is all zeroes, which is the unrealistically benign "+
			"model this arm exists to avoid (ALICE); got %q", rest)
	}
}

// TestSimDisk_TornAppendIsDeterministic pins reproducibility: the same seed and
// the same arm must produce byte-identical garbage, or a failing run cannot be
// replayed.
func TestSimDisk_TornAppendIsDeterministic(t *testing.T) {
	run := func() []byte {
		d := NewSimDisk(NewSeed(11), 0)
		h := tornOpen(t, d, "/wal")
		if _, err := h.Write(bytes.Repeat([]byte{'X'}, 64)); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := h.Sync(); err != nil {
			t.Fatalf("Sync: %v", err)
		}
		d.ArmTornAppendAt(1, 5)
		if _, err := h.Write(bytes.Repeat([]byte{'Y'}, 40)); err != nil {
			t.Fatalf("write: %v", err)
		}
		_ = h.Close()
		d.CrashHost()
		return tornRead(t, d, "/wal")
	}
	a, b := run(), run()
	if !bytes.Equal(a, b) {
		t.Errorf("two runs of one seed produced different torn tails; a torn append must be "+
			"reproducible or a failure cannot be replayed\n a=%q\n b=%q", a, b)
	}
}

// TestSimDisk_TornAppendNeverDiscardsDurableBytes is the safety bound: an arm
// whose kept prefix lies BELOW the durable watermark must not shorten the file,
// because an interrupted write-back cannot un-harden what an fsync already
// hardened.
func TestSimDisk_TornAppendNeverDiscardsDurableBytes(t *testing.T) {
	d := NewSimDisk(NewSeed(3), 0)
	h := tornOpen(t, d, "/wal")

	d.ArmTornAppendAt(1, 4)
	payload := bytes.Repeat([]byte{'Z'}, 40)
	if _, err := h.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Sync AFTER the torn mark: the whole record is now durable, so the tear must
	// not apply.
	if err := h.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	_ = h.Close()
	d.CrashHost()

	got := tornRead(t, d, "/wal")
	if !bytes.Equal(got, payload) {
		t.Errorf("a torn append below the durable watermark changed the file: got %d bytes %q, "+
			"want the %d durable bytes intact", len(got), got, len(payload))
	}
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return len(b) > 0
}

// TestSimStore_RecoveryAgainstAnEmergentTornTail is the acceptance criterion
// proper: a torn trailing frame arises from a SIMULATED CRASH, with no
// out-of-band truncation anywhere in the setup, and real WAL recovery is driven
// against it.
//
// This is what the harness could not do before. It manufactured a tail by
// truncating the byte image directly, so recovery was tested against a tail the
// simulator chose rather than one a crash produced — and a chosen tail is always
// a clean cut at a boundary somebody picked, which is precisely the case a
// recovery reader is least likely to get wrong.
func TestSimStore_RecoveryAgainstAnEmergentTornTail(t *testing.T) {
	disk := NewSimDisk(NewSeed(4242), 0)

	s, err := OpenSimStore(disk, defaultSimStoreConfig())
	if err != nil {
		t.Fatalf("OpenSimStore: %v", err)
	}

	// Three committed-and-synced writes. These are durable and must survive.
	runWrite(t, s, "CREATE (:T {k:1})")
	runWrite(t, s, "CREATE (:T {k:2})")
	runWrite(t, s, "CREATE (:T {k:3})")

	// Tear the very next frame the WAL appends: the fourth write's record
	// reaches the platter only partly before the power goes.
	disk.ArmTornAppendAt(1, 6)
	runWrite(t, s, "CREATE (:T {k:4})")
	s.Crash()

	// Recovery over the torn image. It must SUCCEED — a torn trailing frame is a
	// benign crash state, not corruption — and it must stop at the last intact
	// record rather than accepting the partial one.
	s2, err := OpenSimStore(disk, defaultSimStoreConfig())
	if err != nil {
		t.Fatalf("recovery over a torn trailing frame failed: %v.\nA truncated tail is the "+
			"crash state a write-ahead log exists to survive; it must be recovered from, not "+
			"refused", err)
	}
	defer func() { _ = s2.Close() }()

	if !s2.Clean() {
		t.Errorf("recovery reported the image as unclean; a torn TRAILING frame is benign and " +
			"must not be classified as corruption")
	}

	// The three durable writes survive. The fourth may or may not, depending on
	// where the tear fell relative to its frame — asserting a specific answer
	// there would pin the WAL's framing rather than its recovery contract. What
	// must hold is that nothing durable was lost and nothing impossible appeared.
	got := scalarInt(t, s2, "MATCH (n:T) RETURN count(n)")
	if got < 3 {
		t.Errorf("post-recovery node count = %d, want at least 3: a torn TRAILING frame must "+
			"never cost an earlier committed-and-synced write", got)
	}
	if got > 4 {
		t.Errorf("post-recovery node count = %d, want at most 4; recovery invented rows", got)
	}

	// NON-VACUITY. Everything above is satisfied by a run in which the arm never
	// fired — recovery of an untorn image also succeeds, is also clean, and also
	// keeps three nodes. So the same scenario is replayed WITHOUT the arm and the
	// two post-crash WAL images are compared: if they are identical, nothing was
	// torn and this test proved nothing about torn tails.
	torn := tornWALImage(t, disk)
	untorn := tornWALImage(t, runTornScenario(t, false))
	if bytes.Equal(torn, untorn) {
		t.Errorf("the armed and unarmed runs left byte-identical WAL images (%d bytes), so the "+
			"tear never happened and every assertion above passed for the wrong reason",
			len(torn))
	}
}

// runTornScenario replays the scenario of
// [TestSimStore_RecoveryAgainstAnEmergentTornTail], with or without the torn
// append, and returns the crashed disk.
func runTornScenario(t *testing.T, arm bool) *SimDisk {
	t.Helper()
	disk := NewSimDisk(NewSeed(4242), 0)
	s, err := OpenSimStore(disk, defaultSimStoreConfig())
	if err != nil {
		t.Fatalf("OpenSimStore: %v", err)
	}
	runWrite(t, s, "CREATE (:T {k:1})")
	runWrite(t, s, "CREATE (:T {k:2})")
	runWrite(t, s, "CREATE (:T {k:3})")
	if arm {
		disk.ArmTornAppendAt(1, 6)
	}
	runWrite(t, s, "CREATE (:T {k:4})")
	s.Crash()
	return disk
}

// tornWALImage returns the WAL byte image on disk, which is what a crash leaves
// and what recovery reads.
func tornWALImage(t *testing.T, disk *SimDisk) []byte {
	t.Helper()
	return tornRead(t, disk, simWALPath)
}
