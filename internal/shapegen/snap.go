package shapegen

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// snap.go implements lazy SNAP dataset loaders for three canonical
// graphs from snap.stanford.edu/data:
//
//   - web-Google       (875 713 nodes, 5 105 039 edges)
//   - soc-LiveJournal1 (4 847 571 nodes, 68 993 773 edges)
//   - cit-HepPh        (34 546 nodes, 421 578 edges)
//
// Each loader fetches the .txt.gz archive over HTTPS the first time
// it is invoked, verifies its SHA-256 against the registered
// canonical digest, and caches the file under the supplied
// cacheDir (default $HOME/.cache/gograph-snap, overridable via
// $GOGRAPH_SNAP_DIR). Subsequent invocations hit the cache and never
// touch the network. The parsed graph is returned as a
// *lpg.Graph[int64, struct{}] over a directed [adjlist] backend.
//
// # Determinism
//
// SNAP edge lists are deterministic byte streams. The parser feeds
// AddEdge in file order; with a fresh [adjlist.Config] this yields
// the same NodeID assignment for the same dataset across machines.
//
// # Network policy
//
// The HTTP client uses a 10 s dial timeout and a 30-minute overall
// transfer budget; both are deliberately tight so a hung CDN does
// not stall CI. A failed fetch returns the wrapped sentinel
// [ErrSNAPOffline]; SHA-256 mismatches return [ErrSNAPChecksumMismatch].
// Test code that needs to behave gracefully when the SNAP CDN is
// unreachable should errors.Is against [ErrSNAPOffline] and call
// t.Skip rather than t.Fatal.

// ErrSNAPOffline wraps every transport-layer failure of the HTTPS
// fetch (DNS, dial timeout, TLS, non-2xx response, body truncation).
// Callers running outside a network-enabled environment should
// errors.Is against it and skip the test.
var ErrSNAPOffline = errors.New("shapegen: SNAP fetch failed; check network connectivity")

// ErrSNAPChecksumMismatch is returned when the cached or freshly
// fetched archive does not match the registered SHA-256.
var ErrSNAPChecksumMismatch = errors.New("shapegen: SNAP archive SHA-256 mismatch")

// ErrSNAPUnknownDataset is returned by [LoadSNAP] for a name that is
// not registered in [SNAPDatasets].
var ErrSNAPUnknownDataset = errors.New("shapegen: unknown SNAP dataset")

// SNAPDataset captures every static fact about one dataset.
type SNAPDataset struct {
	// Name is the SNAP archive basename (without the .txt.gz suffix)
	// and the key under which the dataset is registered.
	Name string

	// URL is the canonical HTTPS download URL.
	URL string

	// SHA256 is the lowercase hexadecimal SHA-256 of the gzip-
	// compressed archive — the *.txt.gz file, not its inflated
	// contents.
	SHA256 string

	// Nodes is the SNAP-published node count.
	Nodes uint64

	// Edges is the SNAP-published directed edge count (i.e. the
	// number of `(FromNodeId, ToNodeId)` pairs in the archive
	// excluding the header comments).
	Edges uint64
}

// SNAPDatasets is the registry of supported SNAP datasets keyed by
// the canonical basename of the archive.
var SNAPDatasets = map[string]SNAPDataset{
	"web-Google": {
		Name:   "web-Google",
		URL:    "https://snap.stanford.edu/data/web-Google.txt.gz",
		SHA256: "bcac0af0471d749f4a8c010bca92b61cf2868a0570741de06892fc062f265ea6",
		Nodes:  875_713,
		Edges:  5_105_039,
	},
	"soc-LiveJournal1": {
		Name:   "soc-LiveJournal1",
		URL:    "https://snap.stanford.edu/data/soc-LiveJournal1.txt.gz",
		SHA256: "d7bcd5a87b88c896c35fdb9611e804c3f4033c39b58c4c9ea3ba53c680d516d8",
		Nodes:  4_847_571,
		Edges:  68_993_773,
	},
	"cit-HepPh": {
		Name:   "cit-HepPh",
		URL:    "https://snap.stanford.edu/data/cit-HepPh.txt.gz",
		SHA256: "917e77b3344aed33fd2d849443c9512b7c528b9dc87251d4245fb3777bbe4128",
		Nodes:  34_546,
		Edges:  421_578,
	},
}

// snapCacheDirEnv is the override for the on-disk cache directory.
// Honouring an env var lets test runners point at a CI-shared
// cache or a tmpfs without touching code.
const snapCacheDirEnv = "GOGRAPH_SNAP_DIR"

// SNAPDefaultCacheDir returns the cache directory the loaders use
// when the caller passes an empty cacheDir. The lookup order is:
//
//  1. $GOGRAPH_SNAP_DIR (if set).
//  2. $HOME/.cache/gograph-snap (POSIX-style XDG fallback).
//  3. $TMPDIR/gograph-snap (last-resort fallback when no home is
//     resolvable — practically only on misconfigured CI runners).
func SNAPDefaultCacheDir() string {
	if dir := os.Getenv(snapCacheDirEnv); dir != "" {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".cache", "gograph-snap")
	}
	return filepath.Join(os.TempDir(), "gograph-snap")
}

// LoadSNAP loads the dataset registered under name. cacheDir = ""
// resolves to [SNAPDefaultCacheDir]. The function is the
// general-purpose entry point; [WebGoogle], [SocLiveJournal1] and
// [CitHepPh] are convenience wrappers.
func LoadSNAP(name, cacheDir string) (*lpg.Graph[int64, struct{}], error) {
	ds, ok := SNAPDatasets[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrSNAPUnknownDataset, name)
	}
	if cacheDir == "" {
		cacheDir = SNAPDefaultCacheDir()
	}
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return nil, err
	}
	cachePath := filepath.Join(cacheDir, ds.Name+".txt.gz")
	if err := ensureSNAPCached(cachePath, ds); err != nil {
		return nil, err
	}
	if err := verifySNAPChecksum(cachePath, ds.SHA256); err != nil {
		return nil, err
	}
	return parseSNAPArchive(cachePath)
}

// WebGoogle loads the web-Google dataset. Equivalent to
// LoadSNAP("web-Google", cacheDir).
func WebGoogle(cacheDir string) (*lpg.Graph[int64, struct{}], error) {
	return LoadSNAP("web-Google", cacheDir)
}

// SocLiveJournal1 loads the soc-LiveJournal1 dataset.
func SocLiveJournal1(cacheDir string) (*lpg.Graph[int64, struct{}], error) {
	return LoadSNAP("soc-LiveJournal1", cacheDir)
}

// CitHepPh loads the cit-HepPh dataset.
func CitHepPh(cacheDir string) (*lpg.Graph[int64, struct{}], error) {
	return LoadSNAP("cit-HepPh", cacheDir)
}

// ensureSNAPCached returns nil when the cache file exists, otherwise
// downloads the archive into a sibling .part file and renames it
// into place on success (so a half-downloaded archive never appears
// at the canonical path).
func ensureSNAPCached(cachePath string, ds SNAPDataset) error {
	if st, err := os.Stat(cachePath); err == nil && !st.IsDir() {
		return nil
	}
	return downloadSNAP(ds.URL, cachePath)
}

// downloadSNAP fetches url into dest atomically. A 10 s dial timeout
// guards against hung connections; an overall 30-minute context
// budget bounds the largest dataset (soc-LiveJournal1 at ~250 MB).
func downloadSNAP(url, dest string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSNAPOffline, err)
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       60 * time.Second,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSNAPOffline, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: HTTP %d", ErrSNAPOffline, resp.StatusCode)
	}
	tmp := dest + ".part"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 -- dest is under the caller-controlled cache directory.
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("%w: %w", ErrSNAPOffline, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// verifySNAPChecksum re-computes the SHA-256 of the file at path
// and returns [ErrSNAPChecksumMismatch] when it diverges from want.
// The 64 KB I/O buffer keeps the hash computation memory-bounded
// regardless of file size.
func verifySNAPChecksum(path, want string) error {
	f, err := os.Open(path) // #nosec G304 -- path is the resolved cache path constructed inside this package.
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	buf := make([]byte, 64*1024)
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("%w: %s (got %s, want %s)", ErrSNAPChecksumMismatch, path, got, want)
	}
	return nil
}

// parseSNAPArchive reads the gzip-compressed SNAP edge list at path
// and materialises it into a *lpg.Graph[int64, struct{}] over a
// directed adjacency list. The header (lines starting with `#`) is
// discarded; remaining lines are split on the first run of tab/space
// characters and parsed as a pair of int64 endpoints.
func parseSNAPArchive(path string) (*lpg.Graph[int64, struct{}], error) {
	f, err := os.Open(path) // #nosec G304 -- path is the resolved cache path constructed inside this package.
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("shapegen: SNAP gzip header: %w", err)
	}
	defer func() { _ = gz.Close() }()
	g := lpg.New[int64, struct{}](adjlist.Config{Directed: true})
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Bytes()
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		from, to, err := splitSNAPEdge(line)
		if err != nil {
			return nil, fmt.Errorf("shapegen: SNAP %s:%d: %w", filepath.Base(path), lineNo, err)
		}
		if err := g.AddEdge(from, to, struct{}{}); err != nil {
			return nil, fmt.Errorf("shapegen: SNAP %s:%d AddEdge(%d,%d): %w",
				filepath.Base(path), lineNo, from, to, err)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("shapegen: SNAP scan: %w", err)
	}
	return g, nil
}

// splitSNAPEdge parses a "FromNodeId TAB ToNodeId" line into two
// int64 endpoints. SNAP edge lists are tab-separated but the parser
// also accepts spaces so it tolerates incidental whitespace from
// hand-edited inputs.
func splitSNAPEdge(line []byte) (from, to int64, err error) {
	idx := indexAnyByte(line, " \t")
	if idx < 0 {
		return 0, 0, fmt.Errorf("missing separator in %q", line)
	}
	from, err = strconv.ParseInt(strings.TrimSpace(string(line[:idx])), 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse from %q: %w", line[:idx], err)
	}
	rest := line[idx+1:]
	// Skip any leading whitespace; the spec allows a single tab but
	// tolerates runs of spaces.
	for len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
		rest = rest[1:]
	}
	end := indexAnyByte(rest, " \t")
	if end < 0 {
		end = len(rest)
	}
	to, err = strconv.ParseInt(strings.TrimSpace(string(rest[:end])), 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse to %q: %w", rest[:end], err)
	}
	return from, to, nil
}

// indexAnyByte returns the index of the first byte in s that equals
// any byte in chars, or -1 if none.
func indexAnyByte(s []byte, chars string) int {
	for i, b := range s {
		if strings.IndexByte(chars, b) >= 0 {
			return i
		}
	}
	return -1
}
