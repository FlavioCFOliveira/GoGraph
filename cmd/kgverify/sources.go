package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// rows is the shape every `rmp graph query` returns: named columns and untyped
// row values. The values are kept as any deliberately, because one of the
// checks is about a value's JSON TYPE — a Task id written as "2494" instead of
// 2494 is the retired identity form that rmp #2612 migrated away from, and
// decoding into a typed struct would erase exactly the evidence needed to catch
// it coming back.
type rows struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

func (r *rows) col(name string) int {
	for i, c := range r.Columns {
		if c == name {
			return i
		}
	}
	return -1
}

// str reads a column as a string, reporting whether it was present and textual.
func (r *rows) str(row []any, idx int) (string, bool) {
	if idx < 0 || idx >= len(row) {
		return "", false
	}
	s, ok := row[idx].(string)
	return s, ok
}

// runCmd executes a command and returns its stdout, folding a non-zero exit
// into an error that carries stderr. Every caller here runs a fixed binary with
// arguments this tool composed itself.
func runCmd(dir, bin string, args ...string) ([]byte, error) {
	cmd := exec.Command(bin, args...) //nolint:gosec // bin is a fixed literal at every call site; args are composed by this tool, never taken from an untrusted source.
	cmd.Dir = dir
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(errBuf.String()))
	}
	return out.Bytes(), nil
}

// graphQuery runs one read-only Cypher query against the roadmap's graph.
func graphQuery(dir, roadmap, cypher string) (*rows, error) {
	out, err := runCmd(dir, "rmp", "graph", "query", "-r", roadmap, "--query", cypher)
	if err != nil {
		return nil, err
	}
	var r rows
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, fmt.Errorf("decode graph reply: %w", err)
	}
	return &r, nil
}

// rmpTask is the subset of an rmp task record these checks compare against.
type rmpTask struct {
	ID     int    `json:"id"`
	Status string `json:"status"`
	Title  string `json:"title"`
	Type   string `json:"type"`
}

// fetchTasks resolves every id against rmp, which is the authority on task
// status; the graph only ever holds a stamped snapshot of it.
//
// `rmp task get` fails the whole batch with exit 4 when any single id is
// unknown, so a failing batch is bisected down to the individual ids. That is
// what separates "the graph names a task rmp has never heard of" from "rmp was
// unreachable" — two findings that must never be confused, because the first is
// a fidelity defect and the second means the checker cannot conclude anything.
func fetchTasks(dir, roadmap string, ids []int) (map[int]rmpTask, []int, error) {
	found := make(map[int]rmpTask, len(ids))
	var missing []int
	var hardErr error

	var fetch func([]int)
	fetch = func(chunk []int) {
		if len(chunk) == 0 || hardErr != nil {
			return
		}
		parts := make([]string, len(chunk))
		for i, id := range chunk {
			parts[i] = fmt.Sprint(id)
		}
		out, err := runCmd(dir, "rmp", "task", "get", "-r", roadmap, strings.Join(parts, ","))
		if err == nil {
			var got []rmpTask
			if jsonErr := json.Unmarshal(out, &got); jsonErr != nil {
				hardErr = fmt.Errorf("decode task reply: %w", jsonErr)
				return
			}
			for _, t := range got {
				found[t.ID] = t
			}
			return
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 4 {
			hardErr = err
			return
		}
		if len(chunk) == 1 {
			missing = append(missing, chunk[0])
			return
		}
		mid := len(chunk) / 2
		fetch(chunk[:mid])
		fetch(chunk[mid:])
	}

	const batch = 60
	for i := 0; i < len(ids); i += batch {
		end := min(i+batch, len(ids))
		fetch(ids[i:end])
	}
	if hardErr != nil {
		return nil, nil, hardErr
	}
	return found, missing, nil
}

// gitTouchedFiles lists the files a revision range changed, keeping only those
// that still exist in the tree. A file deleted by the range has no declarations
// left to stamp; a node still pointing at it is caught by the file-existence
// check instead.
func gitTouchedFiles(dir, since string, inv *inventory) (map[string]struct{}, error) {
	out, err := runCmd(dir, "git", "diff", "--name-only", since+"..HEAD")
	if err != nil {
		return nil, err
	}
	files := make(map[string]struct{})
	for _, line := range strings.Split(string(out), "\n") {
		p := strings.TrimSpace(line)
		if p == "" || !strings.HasSuffix(p, ".go") {
			continue
		}
		if inv.hasFile(p) {
			files[p] = struct{}{}
		}
	}
	return files, nil
}

// gitDefaultSince picks the revision the sprint's work started from: the merge
// base with develop when there is one, and the previous commit otherwise.
func gitDefaultSince(dir string) string {
	if out, err := runCmd(dir, "git", "merge-base", "develop", "HEAD"); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return s
		}
	}
	return "HEAD~1"
}

func gitDescribeHead(dir string) (hash, branch string) {
	if out, err := runCmd(dir, "git", "rev-parse", "--short=8", "HEAD"); err == nil {
		hash = strings.TrimSpace(string(out))
	}
	if out, err := runCmd(dir, "git", "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		branch = strings.TrimSpace(string(out))
	}
	return hash, branch
}

func gitTopLevel(dir string) (string, error) {
	out, err := runCmd(dir, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// modulePathOf reads the module path from go.mod, so import paths are derived
// from the module's own declaration rather than assumed.
func modulePathOf(repo string) (string, error) {
	raw, err := runCmd(repo, "go", "list", "-m")
	if err == nil {
		if s := strings.TrimSpace(string(raw)); s != "" {
			return strings.SplitN(s, "\n", 2)[0], nil
		}
	}
	data, err := os.ReadFile(filepath.Join(repo, "go.mod")) //nolint:gosec // repo is this tool's own audit root, resolved from git rev-parse.
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", errors.New("no module directive in go.mod")
}
