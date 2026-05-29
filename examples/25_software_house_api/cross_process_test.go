package main

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests build the server binary and drive it as a real OS process,
// proving that committed state survives a process restart — both a
// graceful SIGTERM (which snapshots and closes the WAL) and a SIGKILL
// (which leaves only the fsynced WAL for recovery). They are skipped when
// `go build` cannot run (e.g. an offline sandbox) and in -short mode.

type serverProc struct {
	cmd  *exec.Cmd
	base string
	err  *bytes.Buffer
}

func buildServerBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "shapi")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = "."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("go build skipped: %v\n%s", err, out)
	}
	return bin
}

// freeAddr reserves and releases a loopback port, returning its address.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// startServer launches the binary against dataDir and blocks until
// /healthz answers 200.
func startServer(t *testing.T, bin, dataDir string, client *http.Client) *serverProc {
	t.Helper()
	addr := freeAddr(t)
	cmd := exec.Command(bin, "-d", dataDir, "-addr", addr)
	cmd.Env = os.Environ()
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	p := &serverProc{cmd: cmd, base: "http://" + addr, err: &errBuf}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(p.base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return p
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	t.Fatalf("server not ready within deadline; stderr:\n%s", errBuf.String())
	return nil
}

// sigterm stops the process gracefully and asserts a clean (exit 0) exit.
func (p *serverProc) sigterm(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	if err := p.cmd.Wait(); err != nil {
		t.Fatalf("graceful shutdown returned error: %v\nstderr:\n%s", err, p.err.String())
	}
}

// kill9 hard-kills the process (no graceful shutdown). A non-zero exit is
// expected and ignored.
func (p *serverProc) kill9() {
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
}

func cpReq(t *testing.T, client *http.Client, method, url, body string) []byte {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s: status %d: %s", method, url, resp.StatusCode, raw)
	}
	return raw
}

func cpDevCount(t *testing.T, client *http.Client, base string) int64 {
	t.Helper()
	var sr statsResponse
	mustJSON(t, cpReq(t, client, http.MethodGet, base+"/stats", ""), &sr)
	return sr.Nodes[typeDeveloper]
}

func newCPClient(t *testing.T) *http.Client {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	t.Cleanup(c.CloseIdleConnections)
	return c
}

func TestCrossProcessGracefulRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-process test launches subprocesses; skipped in short mode")
	}
	bin := buildServerBinary(t)
	dataDir := t.TempDir()
	client := newCPClient(t)

	// Process 1: seed + one write, then a graceful SIGTERM (snapshot + WAL close).
	p1 := startServer(t, bin, dataDir, client)
	cpReq(t, client, http.MethodPost, p1.base+"/seed", "")
	cpReq(t, client, http.MethodPost, p1.base+"/query",
		`{"query":"CREATE (d:Developer:People {key:'dev:zoe', name:'Zoe'})"}`)
	if got := cpDevCount(t, client, p1.base); got != 7 {
		p1.sigterm(t)
		t.Fatalf("Developer count in process 1 = %d, want 7", got)
	}
	p1.sigterm(t)

	// Process 2: fresh start on the same data dir; state must survive.
	p2 := startServer(t, bin, dataDir, client)
	defer p2.sigterm(t)
	if got := cpDevCount(t, client, p2.base); got != 7 {
		t.Errorf("Developer count after graceful restart = %d, want 7 (state lost)", got)
	}
	// A re-seed must be a no-op, proving the seed itself survived.
	var seedResp map[string]any
	mustJSON(t, cpReq(t, client, http.MethodPost, p2.base+"/seed", ""), &seedResp)
	if seedResp["seeded"] != false {
		t.Errorf("re-seed after restart: seeded=%v, want false", seedResp["seeded"])
	}
}

func TestCrossProcessKill9Recovery(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-process kill -9 test launches subprocesses; skipped in short mode")
	}
	bin := buildServerBinary(t)
	dataDir := t.TempDir()
	client := newCPClient(t)

	// Process 1: seed + write, then SIGKILL — no graceful shutdown and no
	// final snapshot. Each committed write was fsynced to the WAL.
	p1 := startServer(t, bin, dataDir, client)
	cpReq(t, client, http.MethodPost, p1.base+"/seed", "")
	cpReq(t, client, http.MethodPost, p1.base+"/query",
		`{"query":"CREATE (d:Developer:People {key:'dev:zoe', name:'Zoe'})"}`)
	if got := cpDevCount(t, client, p1.base); got != 7 {
		p1.kill9()
		t.Fatalf("Developer count in process 1 = %d, want 7", got)
	}
	p1.kill9()

	// Process 2: recovery replays the WAL on top of the initial empty
	// snapshot; the seed and the write must both be present.
	p2 := startServer(t, bin, dataDir, client)
	defer p2.sigterm(t)
	if got := cpDevCount(t, client, p2.base); got != 7 {
		t.Errorf("Developer count after kill -9 recovery = %d, want 7 (WAL not recovered)", got)
	}
}
