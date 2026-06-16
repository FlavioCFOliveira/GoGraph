package sim

import (
	"context"
	"fmt"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// wireExchange is one decoded request/response pair captured from a lock-step
// run, rendered to a stable string for byte-for-byte comparison across runs.
type wireExchange struct {
	op       string
	response string
}

// runLockStepSession drives a deterministic single-connection lock-step session:
// for the given seed it draws a fixed sequence of honest write/read ops, sends
// each over the wire, and captures the terminal response. Because exactly one
// exchange is in flight at a time, the captured stream is a pure function of the
// seed and the (fixed) server engine, so two runs with the same seed produce an
// identical transcript.
func runLockStepSession(t *testing.T, seed uint64, nOps int) []wireExchange {
	t.Helper()
	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	s := NewSeed(seed)
	oracle := NewGraphOracle()
	writer := HonestWriter{}
	transcript := make([]wireExchange, 0, nOps)

	for i := 0; i < nOps; i++ {
		op := writer.NextOp(s, oracle)
		runResp, err := c.Run(op.Cypher, op.Params)
		if err != nil {
			t.Fatalf("RUN op %d: %v", i, err)
		}
		term := any(runResp)
		if _, isFail := runResp.(*proto.Failure); !isFail {
			// Drain the result so the next op starts from a clean READY state.
			_, term, err = c.PullAll()
			if err != nil {
				t.Fatalf("PULL op %d: %v", i, err)
			}
		}
		transcript = append(transcript, wireExchange{
			op:       fmt.Sprintf("%s|%s", op.Kind, op.Cypher),
			response: renderResponse(term),
		})
		// Advance the oracle so subsequent draws reference created names, keeping
		// the op stream a faithful function of the seed.
		applyOracle(oracle, op, !isFailure(term))
	}
	return transcript
}

// renderResponse renders a terminal response to a stable, comparable string.
func renderResponse(term any) string {
	switch m := term.(type) {
	case *proto.Success:
		return "SUCCESS"
	case *proto.Failure:
		return "FAILURE:" + m.Code
	case *proto.Ignored:
		return "IGNORED"
	default:
		return fmt.Sprintf("OTHER:%T", term)
	}
}

func isFailure(term any) bool {
	_, ok := term.(*proto.Failure)
	return ok
}

// applyOracle mirrors the simulator's oracle update for the lock-step transcript
// so the op draw sequence references live names.
func applyOracle(oracle *GraphOracle, op Op, committed bool) {
	if !committed {
		return
	}
	switch op.Kind {
	case OpCreate:
		oracle.ApplyCreate(op.Cypher, op.Params)
	case OpMerge:
		oracle.ApplyMerge(op.Cypher, op.Params)
	case OpDelete:
		oracle.ApplyDelete(op.Cypher, op.Params)
	case OpUpdate, OpMatch:
		oracle.ApplyMatch(op.Cypher, op.Params)
	}
}

// TestWire_LockStepReproducible proves the single-connection lock-step wire path
// is deterministic: the same seed yields a byte-identical op stream AND identical
// terminal responses across two independent runs. This is the determinism proof
// the gate requires for the lock-step mode.
func TestWire_LockStepReproducible(t *testing.T) {
	t.Parallel()
	const seed = 0xC0FFEE
	const nOps = 40

	first := runLockStepSession(t, seed, nOps)
	second := runLockStepSession(t, seed, nOps)

	if len(first) != len(second) {
		t.Fatalf("transcript length differs: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("transcript diverged at op %d:\n  run1: %+v\n  run2: %+v", i, first[i], second[i])
		}
	}
}

// TestWire_LockStepGoleak proves a lock-step session leaks no goroutine after the
// server is closed.
func TestWire_LockStepGoleak(t *testing.T) {
	defer goleak.VerifyNone(t)
	_ = runLockStepSession(t, 0xABCDEF, 25)
}
