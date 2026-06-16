package sim

import (
	"context"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// WireExchange is one decoded request/response pair from a lock-step wire
// session, rendered to stable strings so two runs can be compared byte-for-byte.
type WireExchange struct {
	// Op identifies the operation sent (kind + cypher).
	Op string
	// Response is the terminal response class (SUCCESS, FAILURE:<code>, …).
	Response string
}

// WireTranscript is the ordered list of exchanges from one lock-step session.
// Two transcripts produced from the same seed must be equal — the determinism
// guarantee of the single-connection lock-step Bolt-wire path.
type WireTranscript struct {
	Seed      uint64
	Exchanges []WireExchange
}

// Equal reports whether two transcripts are byte-identical (same length, same
// ordered exchanges). It is the reproducibility predicate.
func (t WireTranscript) Equal(other WireTranscript) bool {
	if len(t.Exchanges) != len(other.Exchanges) {
		return false
	}
	for i := range t.Exchanges {
		if t.Exchanges[i] != other.Exchanges[i] {
			return false
		}
	}
	return true
}

// RunLockStepWire drives a deterministic single-connection LOCK-STEP session
// against a fresh real bolt/server: for the given seed it draws a fixed sequence
// of nOps honest write operations, sends each over the wire, blocks for the
// terminal response, and records the exchange. Because exactly one exchange is
// in flight at a time, the transcript is a pure function of the seed, so two
// calls with the same seed return equal transcripts (assert with
// [WireTranscript.Equal]).
//
// It is the engine behind the cmd/sim `--mode wire` reproducibility demo and the
// determinism proof for the lock-step path.
func RunLockStepWire(seed uint64, nOps int) (WireTranscript, error) {
	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		return WireTranscript{}, err
	}
	defer func() { _ = srv.Close() }()

	c, err := srv.Dial()
	if err != nil {
		return WireTranscript{}, err
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(context.Background()); err != nil {
		return WireTranscript{}, fmt.Errorf("sim: lock-step connect: %w", err)
	}

	s := NewSeed(seed)
	oracle := NewGraphOracle()
	writer := HonestWriter{}
	tr := WireTranscript{Seed: seed, Exchanges: make([]WireExchange, 0, nOps)}

	for i := 0; i < nOps; i++ {
		op := writer.NextOp(s, oracle)
		runResp, err := c.Run(op.Cypher, op.Params)
		if err != nil {
			return tr, fmt.Errorf("sim: lock-step RUN op %d: %w", i, err)
		}
		term := any(runResp)
		isFail := false
		if _, ok := runResp.(*proto.Failure); ok {
			isFail = true
		} else {
			// Drain the result so the next op starts from a clean READY state.
			_, term, err = c.PullAll()
			if err != nil {
				return tr, fmt.Errorf("sim: lock-step PULL op %d: %w", i, err)
			}
			if _, ok := term.(*proto.Failure); ok {
				isFail = true
			}
		}
		tr.Exchanges = append(tr.Exchanges, WireExchange{
			Op:       fmt.Sprintf("%s|%s", op.Kind, op.Cypher),
			Response: renderWireResponse(term),
		})
		applyOracleForLockStep(oracle, op, !isFail)
	}
	return tr, nil
}

// renderWireResponse renders a terminal response to a stable, comparable string.
func renderWireResponse(term any) string {
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

// applyOracleForLockStep advances the oracle for a committed op so subsequent
// draws reference live names, keeping the op stream a faithful function of the
// seed.
func applyOracleForLockStep(oracle *GraphOracle, op Op, committed bool) {
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
