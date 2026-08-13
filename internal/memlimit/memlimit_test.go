package memlimit

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestReadCgroupLimit_ParsesTheShapesTheKernelWrites covers every form the two
// cgroup generations produce, including the two that mean "no limit" and must NOT
// be reported as a bound. Reporting one of those as a limit would derive a
// ceiling from a sentinel — a ceiling of roughly half of 2^62 is not a bound, but
// it would silently look like one.
func TestReadCgroupLimit_ParsesTheShapesTheKernelWrites(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		content  string
		want     int64
		wantOK   bool
		whatItIs string
	}{
		{"v2 real limit", "8589934592\n", 8589934592, true, "an 8 GiB container"},
		{"v2 unlimited", "max\n", 0, false, "cgroup v2's spelling of no limit"},
		{"v1 real limit", "2147483648", 2147483648, true, "a 2 GiB container"},
		{"v1 unlimited sentinel", "9223372036854771712\n", 0, false, "cgroup v1's page-aligned maximum"},
		{"empty", "", 0, false, "a mounted but unpopulated file"},
		{"garbage", "not-a-number\n", 0, false, "a file that is not what we expect"},
		{"zero", "0\n", 0, false, "a limit of zero is not a usable bound"},
		{"negative", "-1\n", 0, false, "never written by the kernel, but must not parse as a bound"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := filepath.Join(t.TempDir(), "limit")
			if err := os.WriteFile(p, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			got, ok := readCgroupLimit(p)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("readCgroupLimit(%q) = (%d, %v), want (%d, %v) — %s",
					tc.content, got, ok, tc.want, tc.wantOK, tc.whatItIs)
			}
		})
	}
}

// TestReadCgroupLimit_MissingFileIsNotAnError is the ordinary case off Linux and
// outside a container, and it must report "no bound" rather than fail.
func TestReadCgroupLimit_MissingFileIsNotAnError(t *testing.T) {
	t.Parallel()
	if v, ok := readCgroupLimit(filepath.Join(t.TempDir(), "absent")); ok {
		t.Errorf("a missing cgroup file reported a limit of %d", v)
	}
}

// TestAvailable_ResolvesOncePerProcess pins the contract that makes this package
// safe to consult from a constructor: the lookup sits behind a filesystem read,
// and a filesystem read that happened per call would be a defect rather than a
// cost. Concurrent callers are included because sync.Once is what guarantees it.
func TestAvailable_ResolvesOncePerProcess(t *testing.T) {
	_, _ = Available() // ensure the one-time resolve has happened
	before := ResolveCount()
	if before == 0 {
		t.Fatal("the lookup never ran, so this test asserts nothing")
	}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = Available()
		}()
	}
	wg.Wait()
	if after := ResolveCount(); after != before {
		t.Errorf("the lookup ran %d more times across 64 concurrent calls; it must run once per process",
			after-before)
	}
}
