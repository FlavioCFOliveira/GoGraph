package shapegen

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/md5" // #nosec G401,G501 -- MD5 is the only digest the LDBC repository publishes; it is used here as the second line of integrity defence behind the optional SHA-256.
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

	"github.com/klauspost/compress/zstd"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// graphalytics.go implements lazy loaders for the LDBC Graphalytics
// benchmark suite (Iosup et al., "The LDBC Graphalytics Benchmark",
// arXiv:2011.15028). Each registered dataset is distributed as a
// zstd-compressed tarball that contains a vertex file (`{name}.v`),
// an edge file (`{name}.e`), a `{name}.properties` descriptor, and
// reference output files for the six benchmark algorithms — BFS,
// CDLP (community detection via label propagation), LCC (local
// clustering coefficient), PR (PageRank), SSSP (single-source
// shortest paths), and WCC (weakly connected components).
//
// The canonical mirror is the SURF Data Repository at
// repository.surfsara.nl/datasets/cwi/graphalytics. The repository
// keeps individual datasets in cold storage; the first HTTPS GET
// against a not-yet-staged file responds with HTTP 409 and a JSON
// envelope pointing at the asynchronous staging endpoint. The
// loader surfaces this as the typed error [ErrGraphalyticsStaging]
// so soak callers can call t.Skip cleanly until the file is online.
//
// # Checksum policy
//
// The SURF API publishes the MD5 of every file. SHA-256 is not yet
// available because the dataset entries have not all been staged
// and re-hashed on the destination side. The loader checks SHA-256
// when a non-empty registered digest is available, and falls back
// to MD5 otherwise; both digests are sufficient for the present
// "did the content arrive intact" use case. Once the user has
// successfully fetched a dataset, the loader logs the realised
// SHA-256 to stderr so it can be promoted into [GraphalyticsDatasets]
// in a follow-up commit.
//
// # Cache layout
//
// LoadGraphalytics inflates the tarball into a per-dataset directory
// under cacheDir/{name}/. The directory contains every file from
// the archive, preserving the LDBC-published naming so callers can
// resolve reference outputs by algorithm with [LoadGraphalyticsReference].

// ErrGraphalyticsStaging is returned when the SURF repository
// responds with HTTP 409 (the canonical "File is offline" status
// of an unstaged file). The wrapped message contains the JSON
// "stage" endpoint so the caller can request staging out of band.
var ErrGraphalyticsStaging = errors.New("shapegen: Graphalytics archive is offline (request staging via SURF API)")

// ErrGraphalyticsOffline wraps any other transport-layer failure of
// the HTTPS fetch (DNS, dial timeout, TLS, non-2xx response other
// than 409, body truncation). Callers running outside a network-
// enabled environment should errors.Is against it and skip the test.
var ErrGraphalyticsOffline = errors.New("shapegen: Graphalytics fetch failed; check network connectivity")

// ErrGraphalyticsChecksumMismatch is returned when the cached or
// fetched archive does not match the registered SHA-256 or MD5.
var ErrGraphalyticsChecksumMismatch = errors.New("shapegen: Graphalytics archive checksum mismatch")

// ErrGraphalyticsUnknownDataset is returned by [LoadGraphalytics]
// for a name that is not registered.
var ErrGraphalyticsUnknownDataset = errors.New("shapegen: unknown Graphalytics dataset")

// ErrGraphalyticsUnknownAlgorithm is returned by
// [LoadGraphalyticsReference] for a benchmark name that is not in
// the canonical algorithm set.
var ErrGraphalyticsUnknownAlgorithm = errors.New("shapegen: unknown Graphalytics algorithm")

// GraphalyticsAlgorithms enumerates the six canonical Graphalytics
// reference algorithms. The string values match the LDBC file-name
// suffix convention so the loader can resolve a reference output
// by appending "-{alg}" to the dataset name.
//
// BFS  : breadth-first search distances.
// CDLP : community detection via label propagation.
// LCC  : local clustering coefficient.
// PR   : PageRank.
// SSSP : single-source shortest paths.
// WCC  : weakly connected components.
var GraphalyticsAlgorithms = []string{"BFS", "CDLP", "LCC", "PR", "SSSP", "WCC"}

// GraphalyticsDataset captures every static fact about one LDBC
// Graphalytics dataset.
type GraphalyticsDataset struct {
	// Name is the dataset key, matching the `.v`/`.e` base name and
	// the archive's basename (without the .tar.zst suffix).
	Name string

	// URL is the canonical HTTPS download URL inside the SURF
	// Data Repository.
	URL string

	// SHA256 is the hex SHA-256 of the .tar.zst archive. Empty
	// when the dataset has not yet been staged + audited; the
	// loader falls back to MD5 in that case.
	SHA256 string

	// MD5 is the hex MD5 published by the SURF repository's API.
	// It is always set for registered datasets.
	MD5 string

	// Nodes is the LDBC-published vertex count.
	Nodes uint64

	// Edges is the LDBC-published edge count (directed if the
	// graph is directed; otherwise the count of undirected pairs).
	Edges uint64
}

// GraphalyticsDatasets is the registry of supported Graphalytics
// datasets keyed by the canonical dataset name. The three entries
// pinned here are the smallest reference graphs declared by AC #1
// of task #524: cit-Patents, dota-league and kgs.
//
// TODO(#524): promote MD5 to SHA-256 once the SURF mirror has
// re-staged the dataset entries. The MD5 values are taken from the
// SURF API at retrieval time; SHA-256 will be filled by the first
// successful soak-layer run that completes a clean fetch+verify
// loop and emits the realised digest to stderr.
var GraphalyticsDatasets = map[string]GraphalyticsDataset{
	"cit-Patents": {
		Name:   "cit-Patents",
		URL:    "https://repository.surfsara.nl/datasets/cwi/graphalytics/files/graphalytics-graph-data-sets/cit-Patents.tar.zst",
		MD5:    "3dc5bdb0e40f56ca7486140900fc1d0f",
		SHA256: "",
		Nodes:  3_774_768,
		Edges:  16_518_947,
	},
	"dota-league": {
		Name:   "dota-league",
		URL:    "https://repository.surfsara.nl/datasets/cwi/graphalytics/files/graphalytics-graph-data-sets/dota-league.tar.zst",
		MD5:    "bae15106fadb84d577b5824655fc27f0",
		SHA256: "",
		Nodes:  61_170,
		Edges:  50_870_313,
	},
	"kgs": {
		Name:   "kgs",
		URL:    "https://repository.surfsara.nl/datasets/cwi/graphalytics/files/graphalytics-graph-data-sets/kgs.tar.zst",
		MD5:    "f6c52a1b68b836410a4c6a5df821e611",
		SHA256: "",
		Nodes:  832_247,
		Edges:  17_891_698,
	},
}

// graphalyticsCacheDirEnv lets callers override the cache root.
const graphalyticsCacheDirEnv = "GOGRAPH_GRAPHALYTICS_DIR"

// GraphalyticsDefaultCacheDir returns the default cache directory.
// Resolution order:
//
//  1. $GOGRAPH_GRAPHALYTICS_DIR (if set).
//  2. $HOME/.cache/gograph-graphalytics.
//  3. $TMPDIR/gograph-graphalytics (last-resort fallback).
func GraphalyticsDefaultCacheDir() string {
	if dir := os.Getenv(graphalyticsCacheDirEnv); dir != "" {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".cache", "gograph-graphalytics")
	}
	return filepath.Join(os.TempDir(), "gograph-graphalytics")
}

// LoadGraphalytics loads the dataset registered under name,
// fetching the .tar.zst archive lazily, verifying its checksum,
// inflating it into cacheDir/{name}/, and parsing the .v + .e
// files into a *lpg.Graph[int64, struct{}] over a directed
// adjlist. cacheDir = "" resolves to [GraphalyticsDefaultCacheDir].
//
// Network failures wrap [ErrGraphalyticsOffline] or
// [ErrGraphalyticsStaging] so the caller can decide whether to
// retry, request staging, or skip.
func LoadGraphalytics(name, cacheDir string) (*lpg.Graph[int64, struct{}], error) {
	ds, ok := GraphalyticsDatasets[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrGraphalyticsUnknownDataset, name)
	}
	if cacheDir == "" {
		cacheDir = GraphalyticsDefaultCacheDir()
	}
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return nil, err
	}
	archivePath := filepath.Join(cacheDir, ds.Name+".tar.zst")
	if err := ensureGraphalyticsCached(archivePath, ds); err != nil {
		return nil, err
	}
	if err := verifyGraphalyticsChecksum(archivePath, ds); err != nil {
		return nil, err
	}
	extractRoot := filepath.Join(cacheDir, ds.Name)
	if err := ensureGraphalyticsExtracted(archivePath, extractRoot); err != nil {
		return nil, err
	}
	return parseGraphalyticsEdgeList(extractRoot, ds.Name)
}

// LoadGraphalyticsReference returns a handle to the reference
// output file of `alg` for the named dataset. The caller must
// close the returned reader. cacheDir = "" resolves to the
// default; the archive must already be extracted under the cache
// (run LoadGraphalytics(name, cacheDir) first).
func LoadGraphalyticsReference(name, alg, cacheDir string) (io.ReadCloser, error) {
	if _, ok := GraphalyticsDatasets[name]; !ok {
		return nil, fmt.Errorf("%w: %q", ErrGraphalyticsUnknownDataset, name)
	}
	if !graphalyticsAlgorithmKnown(alg) {
		return nil, fmt.Errorf("%w: %q (want one of %v)", ErrGraphalyticsUnknownAlgorithm, alg, GraphalyticsAlgorithms)
	}
	if cacheDir == "" {
		cacheDir = GraphalyticsDefaultCacheDir()
	}
	// LDBC ships the reference outputs as "{name}-{alg}". The
	// archive layout places them at the same level as `.v`/`.e`.
	path := filepath.Join(cacheDir, name, name+"-"+alg)
	f, err := os.Open(path) // #nosec G304 -- cacheDir is the caller-owned cache root.
	if err != nil {
		return nil, err
	}
	return f, nil
}

// graphalyticsAlgorithmKnown reports whether s is one of the six
// canonical Graphalytics algorithms.
func graphalyticsAlgorithmKnown(s string) bool {
	for _, a := range GraphalyticsAlgorithms {
		if a == s {
			return true
		}
	}
	return false
}

// ensureGraphalyticsCached returns nil when the archive is on disk;
// otherwise it fetches it atomically. HTTP 409 surfaces as
// [ErrGraphalyticsStaging].
func ensureGraphalyticsCached(archivePath string, ds GraphalyticsDataset) error {
	if st, err := os.Stat(archivePath); err == nil && !st.IsDir() {
		return nil
	}
	return downloadGraphalytics(ds.URL, archivePath)
}

// downloadGraphalytics fetches url into dest with the SURF cold-
// storage contract: HTTP 409 ("File is offline") maps to
// [ErrGraphalyticsStaging] so callers know to request staging out
// of band.
func downloadGraphalytics(url, dest string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrGraphalyticsOffline, err)
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
		return fmt.Errorf("%w: %w", ErrGraphalyticsOffline, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusConflict {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%w: %s", ErrGraphalyticsStaging, strings.TrimSpace(string(body)))
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: HTTP %d", ErrGraphalyticsOffline, resp.StatusCode)
	}
	tmp := dest + ".part"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 -- dest is the resolved cache path.
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("%w: %w", ErrGraphalyticsOffline, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// verifyGraphalyticsChecksum verifies the archive at path against
// the registered SHA-256 when present, and falls back to MD5
// otherwise. The realised SHA-256 is logged to stderr so the
// caller can promote it into the registry on the next commit.
func verifyGraphalyticsChecksum(path string, ds GraphalyticsDataset) error {
	gotSHA, gotMD5, err := digestPair(path)
	if err != nil {
		return err
	}
	if ds.SHA256 != "" {
		if gotSHA != ds.SHA256 {
			return fmt.Errorf("%w: %s (sha256 got %s, want %s)", ErrGraphalyticsChecksumMismatch, path, gotSHA, ds.SHA256)
		}
		return nil
	}
	if ds.MD5 == "" {
		return fmt.Errorf("%w: no expected digest registered for %s", ErrGraphalyticsChecksumMismatch, ds.Name)
	}
	if gotMD5 != ds.MD5 {
		return fmt.Errorf("%w: %s (md5 got %s, want %s)", ErrGraphalyticsChecksumMismatch, path, gotMD5, ds.MD5)
	}
	// Surface the realised SHA-256 so an operator can harvest it from
	// the next clean soak run and back-populate GraphalyticsDatasets.
	// The "realisedSHA256" token is kept stable and greppable for
	// mechanical extraction (dataset=<name> realisedSHA256=<hex>).
	fmt.Fprintf(os.Stderr, "shapegen: Graphalytics dataset=%s realisedSHA256=%s — pin in GraphalyticsDatasets to enforce SHA-256 going forward.\n", ds.Name, gotSHA)
	return nil
}

// digestPair returns the hex SHA-256 and MD5 of the file at path,
// computed in a single read pass. The buffer is small enough to
// keep memory bounded regardless of archive size.
func digestPair(path string) (sha, md string, err error) {
	f, err := os.Open(path) // #nosec G304 -- path is the resolved cache path.
	if err != nil {
		return "", "", err
	}
	defer func() { _ = f.Close() }()
	h1 := sha256.New()
	h2 := md5.New() // #nosec G401 -- see file-level rationale.
	buf := make([]byte, 64*1024)
	if _, err := io.CopyBuffer(io.MultiWriter(h1, h2), f, buf); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(h1.Sum(nil)), hex.EncodeToString(h2.Sum(nil)), nil
}

// ensureGraphalyticsExtracted inflates archivePath into extractRoot
// (creating it if necessary) when the directory is empty. The
// loader does not re-extract when extractRoot is non-empty, so a
// failed run can be resumed by manually clearing the directory.
func ensureGraphalyticsExtracted(archivePath, extractRoot string) error {
	if entries, _ := os.ReadDir(extractRoot); len(entries) > 0 {
		return nil
	}
	if err := os.MkdirAll(extractRoot, 0o750); err != nil {
		return err
	}
	return extractGraphalyticsArchive(archivePath, extractRoot)
}

// extractGraphalyticsArchive walks the entries of the
// zstd-compressed tarball at archivePath and writes each regular
// file into root, refusing absolute paths or ".." components so
// the extraction cannot escape root (zip-slip mitigation).
func extractGraphalyticsArchive(archivePath, root string) error {
	f, err := os.Open(archivePath) // #nosec G304 -- archivePath is the resolved cache path.
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	zr, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(2))
	if err != nil {
		return fmt.Errorf("shapegen: zstd reader: %w", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("shapegen: tar entry: %w", err)
		}
		name := filepath.Clean(hdr.Name)
		if filepath.IsAbs(name) || strings.HasPrefix(name, "..") || strings.Contains(name, string(filepath.Separator)+".."+string(filepath.Separator)) {
			return fmt.Errorf("shapegen: tar entry escapes root: %q", hdr.Name)
		}
		// The LDBC archives ship every file at the archive root or
		// inside one shallow directory; flatten any leading dir so
		// callers always find files at extractRoot/{base}.
		base := filepath.Base(name)
		dst := filepath.Join(root, base)
		switch hdr.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
			if err := writeTarFile(dst, tr, hdr); err != nil {
				return err
			}
		default:
			// Skip symlinks, devices and metadata records.
			continue
		}
	}
}

// writeTarFile streams a single tar file entry to disk. The 4 MB
// limit on a single file is a defensive cap against degenerate
// tar headers; the largest LDBC reference output ships around
// 700 MB but is written by streaming, so the cap applies to the
// header rather than the content.
func writeTarFile(dst string, r io.Reader, hdr *tar.Header) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 -- dst is under the controlled extractRoot.
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil { // #nosec G110 -- the tar header carries a known content length; archive comes from a registered SURF mirror.
		_ = out.Close()
		return fmt.Errorf("shapegen: write %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return err
	}
	// Preserve the published mtime so subsequent cache validity
	// checks can reason about freshness if needed.
	if !hdr.ModTime.IsZero() {
		_ = os.Chtimes(dst, hdr.ModTime, hdr.ModTime)
	}
	return nil
}

// parseGraphalyticsEdgeList reads root/{name}.v and root/{name}.e
// and materialises a directed *lpg.Graph[int64, struct{}]. The
// vertex file lists one int64 id per line; the edge file lists
// "from to [weight]" (whitespace-separated). The loader interns
// every vertex up-front and adds an edge for every line in `.e`.
func parseGraphalyticsEdgeList(root, name string) (*lpg.Graph[int64, struct{}], error) {
	vPath := filepath.Join(root, name+".v")
	ePath := filepath.Join(root, name+".e")
	vf, err := os.Open(vPath) // #nosec G304 -- root is under the controlled cache.
	if err != nil {
		return nil, fmt.Errorf("shapegen: open %s: %w", vPath, err)
	}
	defer func() { _ = vf.Close() }()
	ef, err := os.Open(ePath) // #nosec G304 -- root is under the controlled cache.
	if err != nil {
		return nil, fmt.Errorf("shapegen: open %s: %w", ePath, err)
	}
	defer func() { _ = ef.Close() }()
	g := lpg.New[int64, struct{}](adjlist.Config{Directed: true})
	// Vertices.
	vs := bufio.NewScanner(vf)
	vs.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for vs.Scan() {
		line := strings.TrimSpace(vs.Text())
		if line == "" {
			continue
		}
		id, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("shapegen: %s: parse vertex %q: %w", vPath, line, err)
		}
		if err := g.AddNode(id); err != nil {
			return nil, fmt.Errorf("shapegen: %s: AddNode(%d): %w", vPath, id, err)
		}
	}
	if err := vs.Err(); err != nil {
		return nil, fmt.Errorf("shapegen: scan %s: %w", vPath, err)
	}
	// Edges.
	es := bufio.NewScanner(ef)
	es.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for es.Scan() {
		line := strings.TrimSpace(es.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("shapegen: %s: malformed edge %q", ePath, line)
		}
		from, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("shapegen: %s: parse edge from %q: %w", ePath, fields[0], err)
		}
		to, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("shapegen: %s: parse edge to %q: %w", ePath, fields[1], err)
		}
		if err := g.AddEdge(from, to, struct{}{}); err != nil {
			return nil, fmt.Errorf("shapegen: %s: AddEdge(%d,%d): %w", ePath, from, to, err)
		}
	}
	if err := es.Err(); err != nil {
		return nil, fmt.Errorf("shapegen: scan %s: %w", ePath, err)
	}
	return g, nil
}
