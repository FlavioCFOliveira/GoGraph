package sim

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// These tests pin the structural invariant behind rmp #2538 (audit finding F4):
// a directory-durability entry in SimDisk.dirs must never outlive the subtree it
// describes. A stale entry whose value is false makes a later CrashHost delete a
// directory that has since been recreated at that path and fully fsync'd, which
// is a durable-data loss no filesystem produces — so the simulator would
// manufacture a durability violation and the oracle would charge it to the
// engine. That is the same false-accusation class as the rename model repaired
// under rmp #2514.
//
// The invariant is checked directly by ghostDirEntries, and the checker's own
// sensitivity is proved below: a hand-injected ghost must make it fire, or it
// would be one more guard that proves nothing.

// ghostDirEntries returns, sorted, every NOT-YET-DURABLE path in d.dirs that no
// longer has a subtree: no file key at the path itself and none under
// path + "/". In the opaque-key model a directory exists exactly while some key
// sits under it (see [SimDisk.Stat]), so such an entry describes a directory that
// is gone — and because its value is false, [SimDisk.CrashHost] will delete
// whatever is created at that path next.
//
// The value matters, which is why this is not simply "every entry with no
// subtree". A DURABLE entry over an empty directory is a legitimate state the
// model produces on purpose: a publish rename that reached stable storage while
// the components inside it never did, exactly as documented on
// rollbackRenamesLocked. Being durable, it is not in the crash's removal pass at
// all, so it can destroy nothing.
//
// It is a test-only observer: it takes d.mu and mutates nothing.
func (d *SimDisk) ghostDirEntries() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var ghosts []string
	for dp, durable := range d.dirs {
		if durable {
			continue
		}
		prefix := dp + "/"
		live := false
		for p := range d.files {
			if p == dp || strings.HasPrefix(p, prefix) {
				live = true
				break
			}
		}
		if !live {
			ghosts = append(ghosts, dp)
		}
	}
	sort.Strings(ghosts)
	return ghosts
}

// requireNoGhostDirs asserts the invariant at a labelled point in a sequence, so
// a failure names the operation that broke it rather than the crash that later
// exploited it.
func requireNoGhostDirs(t *testing.T, d *SimDisk, after string) {
	t.Helper()
	if ghosts := d.ghostDirEntries(); len(ghosts) > 0 {
		t.Fatalf("after %s: a non-durable d.dirs entry outlived its subtree for %v; a later crash would delete a recreated, fully-fsync'd directory", after, ghosts)
	}
}

// TestSimDisk_RecreatedDirAfterRemoveSurvivesCrash encodes the audit's R1
// controlled experiment (docs/audits/simdisk-crash-model-fidelity-2026-08-18.md,
// finding F4). Both arms build the SAME fully durable file — written, Sync'd, and
// its dirent hardened by a fsync of its parent — and differ only in whether an
// unrelated predecessor directory was once published onto that path by rename
// and then removed. The predecessor left d.dirs["dir/live"] = false behind, and
// on the unfixed disk the crash honoured that stale entry and deleted the
// subtree: the ghost arm lost the file while the control kept it.
func TestSimDisk_RecreatedDirAfterRemoveSurvivesCrash(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ghost bool
	}{
		{name: "control_path_never_published", ghost: false},
		{name: "path_published_by_rename_then_removed", ghost: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewSimDisk(NewSeed(1), 0)
			if tc.ghost {
				// A predecessor is published onto dir/live by a directory
				// rename, which registers dir/live with a not-yet-durable
				// dirent, and is then removed outright — exactly what the
				// snapshot writer does to a stale staging or backup tree.
				writeFile(t, d, "dir/stage/old", []byte("predecessor"))
				if err := d.Rename("dir/stage", "dir/live"); err != nil {
					t.Fatalf("Rename: %v", err)
				}
				if err := d.RemoveAll("dir/live"); err != nil {
					t.Fatalf("RemoveAll: %v", err)
				}
			}
			// The subject: created in place, data made durable by Sync, dirent
			// made durable by a fsync of its own parent. Nothing about it is
			// non-durable, so no crash model may lose it.
			writeFile(t, d, "dir/live/x", []byte("payload"))
			if err := d.DirSync("dir/live"); err != nil {
				t.Fatalf("DirSync: %v", err)
			}
			d.Crash()
			if !d.Exists("dir/live/x") {
				t.Fatal("a fully written, Sync'd and dirent-fsync'd file was destroyed by a crash: the disk model manufactured a durability violation")
			}
			b, err := d.ReadFile("dir/live/x")
			if err != nil || string(b) != "payload" {
				t.Fatalf("ReadFile after crash = %q, %v; want \"payload\", nil", b, err)
			}
		})
	}
}

// TestSimDisk_DirsNeverOutliveTheirSubtree drives every operation that can drop a
// name — whole-subtree removal, single-name removal, and both kinds of rename,
// which drop a name from the source directory — and asserts the invariant after
// each, so a future call site fails here rather than surfacing as a phantom
// durability violation in a scenario.
func TestSimDisk_DirsNeverOutliveTheirSubtree(t *testing.T) {
	// publish stages a directory holding one file and renames it onto live,
	// which is the only operation that creates a d.dirs entry.
	publish := func(t *testing.T, d *SimDisk, stage, live string) {
		t.Helper()
		writeFile(t, d, stage+"/f", []byte("data"))
		if err := d.Rename(stage, live); err != nil {
			t.Fatalf("Rename(%s, %s): %v", stage, live, err)
		}
	}

	for _, tc := range []struct {
		name string
		run  func(t *testing.T, d *SimDisk)
	}{
		{
			name: "RemoveAll of the published directory",
			run: func(t *testing.T, d *SimDisk) {
				publish(t, d, "db/tmp", "db/live")
				if err := d.RemoveAll("db/live"); err != nil {
					t.Fatalf("RemoveAll: %v", err)
				}
			},
		},
		{
			name: "RemoveAll of an ancestor of the published directory",
			run: func(t *testing.T, d *SimDisk) {
				publish(t, d, "db/tmp", "db/live")
				if err := d.RemoveAll("db"); err != nil {
					t.Fatalf("RemoveAll: %v", err)
				}
			},
		},
		{
			name: "Rename replacing an existing published destination",
			run: func(t *testing.T, d *SimDisk) {
				publish(t, d, "db/tmp1", "db/live")
				// A second publish onto the same live name replaces the first,
				// unlinking every name inside it.
				publish(t, d, "db/tmp2", "db/live")
			},
		},
		{
			name: "Rename of a directory holding a published subdirectory",
			run: func(t *testing.T, d *SimDisk) {
				publish(t, d, "db/tmp", "db/live/inner")
				if err := d.Rename("db/live", "db/archive"); err != nil {
					t.Fatalf("Rename: %v", err)
				}
			},
		},
		{
			name: "Rename moving the last file out of a published directory",
			run: func(t *testing.T, d *SimDisk) {
				publish(t, d, "db/tmp", "db/live")
				// A cross-directory single-file rename empties db/live. This is
				// the route the #2538 review measured as still live after the
				// first fix: the entry survived with its value false and the
				// next crash destroyed whatever had been created there.
				if err := d.Rename("db/live/f", "db/elsewhere/f"); err != nil {
					t.Fatalf("Rename: %v", err)
				}
			},
		},
		{
			name: "Rename within the published directory keeps its entry",
			run: func(t *testing.T, d *SimDisk) {
				publish(t, d, "db/tmp", "db/live")
				// The shape every engine rename actually has (path+".tmp" ->
				// path): the directory still holds a name, so nothing is pruned
				// and the publish's own non-durability must be preserved.
				if err := d.Rename("db/live/f", "db/live/f2"); err != nil {
					t.Fatalf("Rename: %v", err)
				}
				d.mu.Lock()
				dur, tracked := d.dirs["db/live"]
				d.mu.Unlock()
				if !tracked || dur {
					t.Fatalf("d.dirs[db/live] = (%v, tracked=%v); want (false, true): a same-directory rename must not forget the publish's non-durability", dur, tracked)
				}
			},
		},
		{
			name: "Remove unlinking the last file inside a published directory",
			run: func(t *testing.T, d *SimDisk) {
				publish(t, d, "db/tmp", "db/live")
				if err := d.Remove("db/live/f"); err != nil {
					t.Fatalf("Remove: %v", err)
				}
			},
		},
		{
			name: "CrashHost dropping a non-durable published directory",
			run: func(t *testing.T, d *SimDisk) {
				publish(t, d, "db/tmp1", "db/live1")
				publish(t, d, "db/tmp2", "db/live2")
				// Removing the second publish pins both undo records into the
				// durable prefix, so the crash reaches its directory pass with
				// nothing to roll back and live1's entry still non-durable.
				// That is the snapshot writer's own shape: publish, then
				// RemoveAll the archive it no longer needs.
				if err := d.RemoveAll("db/live2"); err != nil {
					t.Fatalf("RemoveAll: %v", err)
				}
				d.CrashHost()
				if d.Exists("db/live1/f") {
					t.Fatal("a publish rename whose dirent was never fsync'd must be lost by a host crash")
				}
			},
		},
		{
			name: "CrashHost keeping a publish rename whose components were never fsync'd",
			run: func(t *testing.T, d *SimDisk) {
				// The write-back arm makes the rename durable at issue time, so
				// the crash keeps the directory while revoking the file inside
				// it: a durable entry over an empty directory, which is a
				// legitimate residue and not a ghost.
				d.ArmRenameWritebackForPath("db/live")
				publish(t, d, "db/tmp", "db/live")
				d.CrashHost()
				if got := d.RenameWritebackCount(); got != 1 {
					t.Fatalf("RenameWritebackCount = %d; want 1: the write-back branch was never selected, so this case asserts nothing", got)
				}
				if d.Exists("db/live/f") {
					t.Fatal("a component created without a directory fsync must not survive, even when the publish rename does")
				}
			},
		},
		{
			name: "CrashHost rolling back the publish rename",
			run: func(t *testing.T, d *SimDisk) {
				// The arm is one-shot and fires on the NEXT rename onto this
				// destination, so it MUST precede the publish; installed after
				// it, the case would degrade into whatever the seeded keep-draw
				// happened to pick. Its counter is asserted below for exactly
				// that reason: an arm that never fires is not an assertion.
				d.ArmRenameRollbackForPath("db/live")
				publish(t, d, "db/tmp", "db/live")
				d.CrashHost()
				if got := d.RenameRollbackCount(); got != 1 {
					t.Fatalf("RenameRollbackCount = %d; want 1: the rollback branch was never selected, so this case asserts nothing", got)
				}
				if d.Exists("db/live/f") {
					t.Fatal("a rolled-back publish rename must not leave the subtree under the new name")
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewSimDisk(NewSeed(7), 0)
			tc.run(t, d)
			requireNoGhostDirs(t, d, tc.name)
		})
	}
}

// TestSimDisk_GhostDirEntriesFires is the sensitivity proof for the invariant
// checker: an oracle that cannot fail proves nothing, so a hand-injected ghost
// must be reported — and the legitimate durable residue must not be.
func TestSimDisk_GhostDirEntriesFires(t *testing.T) {
	d := NewSimDisk(NewSeed(1), 0)
	writeFile(t, d, "db/live/f", []byte("data"))
	if ghosts := d.ghostDirEntries(); len(ghosts) != 0 {
		t.Fatalf("ghostDirEntries on a clean disk = %v; want none", ghosts)
	}
	// Inject exactly the state the fix makes unreachable: a non-durable entry
	// for a path with no subtree.
	d.mu.Lock()
	d.dirs["db/gone"] = false
	// ... and the durable residue the checker must tolerate.
	d.dirs["db/kept"] = true
	d.mu.Unlock()
	ghosts := d.ghostDirEntries()
	if len(ghosts) != 1 || ghosts[0] != "db/gone" {
		t.Fatalf("ghostDirEntries = %v; want [db/gone] only (a durable entry over an empty directory is legitimate)", ghosts)
	}
}

// TestSimDisk_DirsDeletionIsCentralised guards the structural half of the fix:
// the audit's point was that the invariant was violable at all, because the
// d.dirs entries were cleared by hand at each call site and two of the three
// sites forgot. Deletion now lives in removeSubtreeLocked, which every removal
// path goes through, plus the source-name handover inside Rename. A new
// hand-deletion anywhere else re-opens the same hole, so this test names the
// permitted sites and fails on any other.
func TestSimDisk_DirsDeletionIsCentralised(t *testing.T) {
	permitted := map[string]string{
		"removeSubtreeLocked":        "the single subtree-removal path: it clears the entry for the removed path and for everything under it",
		"pruneGhostDirEntriesLocked": "the single-name removal path, which cannot go through removeSubtreeLocked",
		"Rename":                     "the source name hands its entry to the destination, and nested entries travel with the subtree",
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "disk.go", nil, 0)
	if err != nil {
		t.Fatalf("parse disk.go: %v", err)
	}

	seen := map[string]int{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || ident.Name != "delete" || len(call.Args) != 2 {
				return true
			}
			sel, ok := call.Args[0].(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "dirs" {
				return true
			}
			if _, ok := permitted[fn.Name.Name]; !ok {
				t.Errorf("%s: %s deletes from d.dirs; keep the removal in removeSubtreeLocked so the invariant \"d.dirs never outlives its subtree\" holds by construction (rmp #2538)",
					fset.Position(call.Pos()), fn.Name.Name)
			}
			seen[fn.Name.Name]++
			return true
		})
	}

	// The guard must be watching something: if the permitted sites stop
	// deleting altogether the list is stale and this test has gone blind.
	if seen["removeSubtreeLocked"] == 0 {
		t.Error("removeSubtreeLocked no longer deletes from d.dirs: the invariant is not maintained where every removal path goes through")
	}
}

// TestSimDisk_RolledBackRenameRestoresPrunedDirEntry proves the pruning of
// #2538's ghost does not buy fidelity in one direction by losing it in the other.
// Moving the last name out of a directory published by rename drops that
// directory's entry, but a crash may roll the rename back and put the name
// straight back in. The entry has to return WITH ITS NON-DURABILITY, or the model
// would keep a subtree whose directory name never reached stable storage — a
// crash losing LESS than a real one, which is the mirror of the false accusation
// the ghost produced.
func TestSimDisk_RolledBackRenameRestoresPrunedDirEntry(t *testing.T) {
	d := NewSimDisk(NewSeed(3), 0)
	writeFile(t, d, "db/tmp/f", []byte("data"))
	if err := d.Rename("db/tmp", "db/live"); err != nil {
		t.Fatalf("publish Rename: %v", err)
	}
	// Harden the FILE's own dirent while never fsyncing the parent of db/live,
	// so the name INSIDE the directory is durable and the directory's own name
	// is not. That asymmetry is what makes the directory entry the deciding
	// fact: without it the crash drops the restored file on its own dirent and
	// the two outcomes are indistinguishable — the first version of this test
	// passed with the restore disabled for exactly that reason.
	if err := d.DirSync("db/live"); err != nil {
		t.Fatalf("DirSync: %v", err)
	}
	// Take the publish rename out of the rollback log without curing its
	// non-durability: an operation on an ANCESTOR pins it into the durable
	// prefix. Left pending, the crash could KEEP it, which flips the entry
	// durable and again collapses the two outcomes into one.
	if err := d.Remove("db"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := d.PendingRenameCount(); got != 0 {
		t.Fatalf("PendingRenameCount = %d; want 0: the publish must not be rollback-able, or the crash can flip its entry durable", got)
	}
	d.ArmRenameRollbackForPath("db/elsewhere/f")
	if err := d.Rename("db/live/f", "db/elsewhere/f"); err != nil {
		t.Fatalf("Rename out: %v", err)
	}
	requireNoGhostDirs(t, d, "a rename that emptied a published directory")

	d.CrashHost()
	if got := d.RenameRollbackCount(); got != 1 {
		t.Fatalf("RenameRollbackCount = %d; want 1: the rollback branch was never selected, so this test asserts nothing", got)
	}
	if d.Exists("db/live/f") || d.Exists("db/elsewhere/f") {
		t.Fatalf("db/live's own name never reached stable storage, so the rolled-back name must be lost with it; live=%v elsewhere=%v",
			d.Exists("db/live/f"), d.Exists("db/elsewhere/f"))
	}
}
