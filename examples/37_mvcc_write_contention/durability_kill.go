package main

// durability_kill.go — D02: kill -9 of a child process committing under concurrent
// writers (rmp #2935). The soak layer runs it five times (durability_soak_test.go);
// the binary runs it with -durability-kill-runs.
//
// The child opens a store, declares the schema, and runs the writers, an open
// holder (D06) and a checkpointer triggered back to back (so a kill can land
// inside any checkpoint phase, D09) until it is killed. It reports every attempt
// on stdout, one line per event, written with one unbuffered write so that a line
// on the pipe means the event happened:
//
//	P <id> <kind>   attempt begun          A <id> <clock>   acknowledged
//	F <id>          refused                B <id>           rolled back
//	X <id>          failed                 O <id>           open at the crash
//
// An "A" line is written only after Commit returned nil, so EVERY "A" line the
// parent reads — before or after the kill — is an acknowledged commit the
// recovered directory owes.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// lineSink writes the child's events to its stdout.
type lineSink struct {
	mu sync.Mutex
	w  io.Writer
	g  *lpg.Graph[string, float64]
}

func (s *lineSink) emit(line string) {
	s.mu.Lock()
	_, _ = io.WriteString(s.w, line)
	s.mu.Unlock()
}

func (s *lineSink) begin(id int64, kind byte) { s.emit(fmt.Sprintf("P %d %c\n", id, kind)) }

func (s *lineSink) end(id int64, o ackOutcome) {
	switch o {
	case oAcked:
		s.emit(fmt.Sprintf("A %d %d\n", id, s.g.MVCCStats().Now))
	case oRefused:
		s.emit(fmt.Sprintf("F %d\n", id))
	case oRolledBack:
		s.emit(fmt.Sprintf("B %d\n", id))
	case oOpen:
		s.emit(fmt.Sprintf("O %d\n", id))
	default:
		s.emit(fmt.Sprintf("X %d\n", id))
	}
}

// runDurabilityChild is the kill child: it commits into dir with level writers
// until it is killed. It returns only on a harness failure.
func runDurabilityChild(ctx context.Context, dir string, level int, w io.Writer) error {
	o, err := store.Open[string, float64](dir, store.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		return err
	}
	eng := cypher.NewEngineWithOpened(o)
	if err := setupSchema(ctx, eng); err != nil {
		return err
	}
	sk := &lineSink{w: w, g: o.Graph()}
	ws := &writerSet{eng: eng, sink: sk}
	var unused sync.Mutex
	cp := checkpoint.New(checkpoint.Config{Dir: dir}, o.Graph(), o.WAL(), &unused,
		checkpoint.WithCommitSerialiser[string, float64](o.Store().RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](txn.NewStringCodec()),
		checkpoint.WithWeightCodec[string, float64](txn.NewFloat64WeightCodec()),
		checkpoint.WithConstraintSpecs[string, float64](eng.ConstraintSpecsForSnapshot),
		checkpoint.WithIndexSpecs[string, float64](eng.IndexSpecsForSnapshot))
	go func() {
		for {
			_ = cp.RunCheckpoint()
			time.Sleep(2 * time.Millisecond)
		}
	}()
	// The open holder (D06) is re-opened every holdFor: a transaction open for the
	// child's whole life would hold every checkpoint's drain — and, with admission
	// closed, every writer — until the kill (txn.Store.RunUnderCommitLock waits for
	// open transactions by contract).
	go func() {
		for {
			opened, release := make(chan struct{}), make(chan struct{})
			go func() { <-opened; time.Sleep(holdFor); close(release) }()
			if err := ws.holdOpen(ctx, opened, release); err != nil {
				fmt.Fprintf(os.Stderr, "holder: %v\n", err)
			}
		}
	}()
	sk.emit("R\n")
	return ws.runWriters(ctx, level, 1<<30, 1)
}

// childCommand is the default kill-child command: this binary in child mode.
func childCommand(ctx context.Context, dir string, level int) *exec.Cmd {
	// os.Args[0] is this example's own binary; the arguments are fixed flags.
	return exec.CommandContext(ctx, os.Args[0], //nolint:gosec // G204: the example's own binary, fixed flags
		"-durability-child-dir", dir, "-durability-child-level", strconv.Itoa(level))
}

// holdFor is how long the kill child keeps each open-holder transaction open.
const holdFor = 5 * time.Millisecond

// killBudget bounds one kill run's wait for its acknowledgements: a child that
// does not reach them in this time is a harness failure, not a slow machine.
const killBudget = 60 * time.Second

// armKill is D02: one kill -9 run. The kill lands after a run-dependent number of
// acknowledgements, so the five runs crash at different points of the workload and
// of the checkpoint cycle.
func armKill(ctx context.Context, dc *durabilityConfig, out *ladderOut, run int) error {
	row := fmt.Sprintf("D02.run%d", run)
	level := dc.killLevel
	dir, err := storeDirFor("D02", level, strconv.Itoa(run))
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	mk := dc.childCmd
	if mk == nil {
		mk = childCommand
	}
	cctx, cancel := context.WithTimeout(ctx, 2*killBudget)
	defer cancel()
	cmd := mk(cctx, dir, level)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	log := newAckLog()
	ready := make(chan struct{})
	eof := make(chan struct{})
	var parseErr error
	go func() {
		defer close(eof)
		sc := bufio.NewScanner(pipe)
		readyOnce := sync.Once{}
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) == 0 {
				continue
			}
			if f[0] == "R" {
				readyOnce.Do(func() { close(ready) })
				continue
			}
			if len(f) < 2 {
				parseErr = fmt.Errorf("child line %q", sc.Text())
				continue
			}
			id, perr := strconv.ParseInt(f[1], 10, 64)
			if perr != nil {
				parseErr = fmt.Errorf("child line %q: %w", sc.Text(), perr)
				continue
			}
			switch f[0] {
			case "P":
				kind := byte('D')
				if len(f) > 2 && f[2] != "" {
					kind = f[2][0]
				}
				log.begin(id, kind)
			case "A":
				if len(f) > 2 {
					if now, cerr := strconv.ParseUint(f[2], 10, 64); cerr == nil {
						log.mu.Lock()
						log.clockMax = max(log.clockMax, now)
						log.mu.Unlock()
					}
				}
				log.end(id, oAcked)
			case "F":
				log.end(id, oRefused)
			case "B":
				log.end(id, oRolledBack)
			case "O":
				log.end(id, oOpen)
			default:
				log.end(id, oFailed)
			}
		}
	}()
	target := 100 + 60*run
	reached := false
	deadline := time.Now().Add(killBudget)
	for time.Now().Before(deadline) {
		if log.acked() >= target {
			reached = true
			break
		}
		select {
		case <-eof:
			deadline = time.Now()
		case <-time.After(time.Millisecond):
		}
	}
	beforeKill := log.acked()
	sizeAtKill, _ := dirSize(dir)
	_ = cmd.Process.Kill() // SIGKILL on Unix
	waitErr := cmd.Wait()
	<-eof
	killed := false
	desc := "exited"
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		desc = ee.ProcessState.String()
		if ws, ok := ee.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL {
			killed = true
		}
	}
	if !reached || !killed || parseErr != nil {
		return fmt.Errorf("%s: child reached %d of %d acknowledgements, termination %q, parse error %v; stderr:\n%s",
			row, beforeKill, target, desc, parseErr, stderr.String())
	}
	select {
	case <-ready:
	default:
		return fmt.Errorf("%s: the child never reported ready", row)
	}
	all := log.acked()
	log.mu.Lock()
	clock := log.clockMax
	log.mu.Unlock()
	// Every "A" line is an acknowledged commit: the owed set is all of them.
	br := bracket{before: all, after: all, clockBefore: clock, walLimit: -1, imageBytes: sizeAtKill}
	out.tele(row, level, "acknowledged_before_kill", beforeKill, "acknowledged_lines_total", all,
		"child_termination", desc, "refused_logged", log.count(oRefused), "rolled_back_logged", log.count(oRolledBack))
	if _, err := verifyImage(ctx, out, row, level, dir, br, log, verifyOpts{fullOpen: true, holes: true}); err != nil {
		return err
	}
	return nil
}
