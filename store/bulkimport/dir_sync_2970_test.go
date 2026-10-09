package bulkimport_test

// dir_sync_2970_test.go — rmp #2970.
//
// A publish that creates its store directory must make every directory entry it
// created durable before it acknowledges: it fsyncs each directory it created,
// deepest first, and then the first ancestor that already existed. Without the
// fsync of the parent, a host crash can drop the store directory's own entry,
// and an acknowledged import recovers empty. The crash consequence is measured
// by the bulkimport-parity fault arm in internal/sim; this test pins which
// directories are synced, and when. The OS-backed path through a nested absent
// directory is driven by TestPublish_AcceptsAbsentAndEmptyDirectories.
//
// Layer: short.

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/store/bulkimport"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
)

// recordingFS is an operating-system filesystem that records the directory
// fsyncs and component creations a publish performs, in order. Its DirSync is
// a record only: the test asserts the call, not the kernel's behaviour.
type recordingFS struct {
	events []string
}

func (r *recordingFS) ReadDir(dir string) ([]fs.DirEntry, error) { return os.ReadDir(dir) }

func (r *recordingFS) MkdirAll(dir string, perm fs.FileMode) error { return os.MkdirAll(dir, perm) }

func (r *recordingFS) Create(path string) (snapshot.File, error) {
	r.events = append(r.events, "create "+path)
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // test temp dir
}

func (r *recordingFS) OpenComponent(path string) (snapshot.ReadFile, error) {
	return os.Open(path) //nolint:gosec // test temp dir
}

func (r *recordingFS) Open(path string) (snapshot.ReadFile, error) {
	return os.Open(path) //nolint:gosec // test temp dir
}

func (r *recordingFS) Rename(oldPath, newPath string) error { return os.Rename(oldPath, newPath) }

func (r *recordingFS) Remove(path string) error { return os.Remove(path) }

func (r *recordingFS) RemoveAll(path string) error { return os.RemoveAll(path) }

func (r *recordingFS) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }

func (r *recordingFS) DirSync(path string) error {
	r.events = append(r.events, "dirsync "+path)
	return nil
}

func (r *recordingFS) ParentDirSync(childPath string) error {
	r.events = append(r.events, "dirsync "+filepath.Dir(childPath))
	return nil
}

// TestPublishFS_FsyncsEveryCreatedDirectoryAndItsParent is the regression test
// for rmp #2970: on the old code no directory fsync preceded the first
// component creation, so neither the created directories nor the pre-existing
// parent were synced by the publish.
func TestPublishFS_FsyncsEveryCreatedDirectoryAndItsParent(t *testing.T) {
	t.Parallel()
	nodes, edges := pubFixture()
	opts := bulkimport.Options{}

	t.Run("nested absent directory", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "a", "b", "store")
		r := &recordingFS{}
		if _, err := bulkimport.ImportIntoFS[int64](context.Background(), r, dir, opts, nodes, edges); err != nil {
			t.Fatalf("ImportIntoFS: %v", err)
		}
		want := []string{
			"dirsync " + dir,
			"dirsync " + filepath.Join(root, "a", "b"),
			"dirsync " + filepath.Join(root, "a"),
			"dirsync " + root,
		}
		if len(r.events) < len(want) || !slices.Equal(r.events[:len(want)], want) {
			t.Fatalf("the publish's first filesystem events are %q, want %q before any component is created",
				r.events[:min(len(r.events), len(want))], want)
		}
		if res := openStore(t, dir); !res.SnapshotHit {
			t.Fatal("the store did not open")
		}
	})

	t.Run("existing empty directory", func(t *testing.T) {
		dir := t.TempDir()
		r := &recordingFS{}
		if _, err := bulkimport.ImportIntoFS[int64](context.Background(), r, dir, opts, nodes, edges); err != nil {
			t.Fatalf("ImportIntoFS: %v", err)
		}
		if len(r.events) == 0 || !strings.HasPrefix(r.events[0], "create ") {
			t.Fatalf("the publish created no directory, yet its first event is %q, want a component creation",
				r.events[:min(len(r.events), 1)])
		}
	})
}
