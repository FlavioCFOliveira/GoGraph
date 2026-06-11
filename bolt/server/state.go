// Package server implements the Bolt v5 TCP server for the GoGraph Cypher
// engine. It handles connection acceptance, Bolt protocol negotiation, session
// lifecycle, and authentication.
//
// # Concurrency
//
// Server is safe for concurrent use by multiple goroutines. Session and State
// are NOT safe for concurrent use; each connection owns exactly one Session.
package server

import (
	"errors"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// State represents the Bolt v5 per-connection protocol state machine state.
type State uint8

const (
	// StateConnected is the initial state: TCP connection established, no
	// protocol negotiation has occurred yet.
	StateConnected State = iota

	// StateNegotiation is reached after version negotiation; the server awaits
	// the client's HELLO message.
	StateNegotiation

	// StateReady is the idle state after a successful HELLO or after a result
	// set has been fully consumed, committed, or rolled back.
	StateReady

	// StateStreaming is active when a query has been run (auto-commit) and
	// records are available to pull.
	StateStreaming

	// StateTxReady is reached after BEGIN; the server awaits RUN, COMMIT, or
	// ROLLBACK within an explicit transaction.
	StateTxReady

	// StateTxStreaming is active when a query has been run inside an explicit
	// transaction and records are available to pull.
	StateTxStreaming

	// StateFailed is entered when a request fails; the server ignores further
	// requests until RESET is received.
	StateFailed

	// StateDefunct is the terminal state: the connection is closed and no
	// further messages are processed.
	StateDefunct
)

// String returns the name of the state for logging and diagnostics.
func (s State) String() string {
	switch s {
	case StateConnected:
		return "CONNECTED"
	case StateNegotiation:
		return "NEGOTIATION"
	case StateReady:
		return "READY"
	case StateStreaming:
		return "STREAMING"
	case StateTxReady:
		return "TX_READY"
	case StateTxStreaming:
		return "TX_STREAMING"
	case StateFailed:
		return "FAILED"
	case StateDefunct:
		return "DEFUNCT"
	default:
		return "UNKNOWN"
	}
}

// ErrInvalidTransition is returned by Transition when the given message type
// is not permitted in the current state.
var ErrInvalidTransition = errors.New("bolt: invalid state transition")

// Transition computes the next state given the current state, the incoming
// message, and whether the operation succeeded.
//
// msg must be one of the pointer types from the proto package (e.g. *proto.Run,
// *proto.Pull, etc.). success indicates whether the server-side operation
// succeeded; on failure the next state is StateFailed (unless the transition
// itself is illegal).
//
// Returns (StateFailed, ErrInvalidTransition) for illegal state/message
// combinations.
//
//nolint:gocyclo // Bolt v5 state machine has O(states×messages) branches; splitting it would obscure the protocol spec.
func Transition(current State, msg any, success bool) (State, error) {
	// GOODBYE and RESET are universal transitions from any non-DEFUNCT state.
	switch msg.(type) {
	case *proto.Goodbye:
		if current == StateDefunct {
			return StateDefunct, ErrInvalidTransition
		}
		return StateDefunct, nil
	case *proto.Reset:
		if current == StateDefunct {
			return StateDefunct, ErrInvalidTransition
		}
		if !success {
			return StateFailed, nil
		}
		// RESET issued before the connection has left the pre-HELLO phase must
		// not advance to READY: it returns to NEGOTIATION so a HELLO is still
		// required. The authoritative authentication gate lives in the session
		// layer ([Session.handleReset] consults [Session.authenticated]); this
		// keeps the transport state machine itself from minting READY out of the
		// pre-authentication phase as a second line of defence. (task #1345)
		if current == StateConnected || current == StateNegotiation {
			return StateNegotiation, nil
		}
		return StateReady, nil
	}

	switch current {
	case StateConnected:
		// No application-level messages are valid in CONNECTED; negotiation is
		// handled at the transport layer before any proto message is decoded.
		return StateFailed, ErrInvalidTransition

	case StateNegotiation:
		switch msg.(type) {
		case *proto.Hello:
			if !success {
				return StateFailed, nil
			}
			return StateReady, nil
		default:
			return StateFailed, ErrInvalidTransition
		}

	case StateReady:
		switch msg.(type) {
		case *proto.Run:
			if !success {
				return StateFailed, nil
			}
			return StateStreaming, nil
		case *proto.Begin:
			if !success {
				return StateFailed, nil
			}
			return StateTxReady, nil
		// LOGON / LOGOFF are allowed in READY for re-authentication.
		case *proto.Logon:
			if !success {
				return StateFailed, nil
			}
			return StateReady, nil
		case *proto.Logoff:
			if !success {
				return StateFailed, nil
			}
			return StateReady, nil
		default:
			return StateFailed, ErrInvalidTransition
		}

	case StateStreaming:
		switch msg.(type) {
		case *proto.Pull:
			if !success {
				return StateFailed, nil
			}
			// The caller sets success=false when there is more data; we use
			// the metadata has_more flag instead. By convention, success=true
			// here means the stream is exhausted and we return to READY.
			// This function does not inspect the result cursor directly; it
			// relies on the caller passing hasMore information via the
			// dedicated HasMoreTransition helper.
			return StateReady, nil
		case *proto.Discard:
			if !success {
				return StateFailed, nil
			}
			return StateReady, nil
		default:
			return StateFailed, ErrInvalidTransition
		}

	case StateTxReady:
		switch msg.(type) {
		case *proto.Run:
			if !success {
				return StateFailed, nil
			}
			return StateTxStreaming, nil
		case *proto.Commit:
			if !success {
				return StateFailed, nil
			}
			return StateReady, nil
		case *proto.Rollback:
			if !success {
				return StateFailed, nil
			}
			return StateReady, nil
		// LOGON / LOGOFF also allowed in TX_READY.
		case *proto.Logon:
			if !success {
				return StateFailed, nil
			}
			return StateTxReady, nil
		case *proto.Logoff:
			if !success {
				return StateFailed, nil
			}
			return StateTxReady, nil
		default:
			return StateFailed, ErrInvalidTransition
		}

	case StateTxStreaming:
		switch msg.(type) {
		case *proto.Pull:
			if !success {
				return StateFailed, nil
			}
			return StateTxReady, nil
		case *proto.Discard:
			if !success {
				return StateFailed, nil
			}
			return StateTxReady, nil
		default:
			return StateFailed, ErrInvalidTransition
		}

	case StateFailed:
		// Only RESET escapes FAILED; handled at the top of this function.
		return StateFailed, ErrInvalidTransition

	case StateDefunct:
		return StateDefunct, ErrInvalidTransition

	default:
		return StateFailed, ErrInvalidTransition
	}
}

// StreamingTransition is a variant of Transition for PULL in STREAMING or
// TX_STREAMING states when there are more records to deliver (has_more=true).
// In that case the connection remains in the same streaming state instead of
// returning to READY/TX_READY.
func StreamingTransition(current State, hasMore bool) (State, error) {
	switch current {
	case StateStreaming:
		if hasMore {
			return StateStreaming, nil
		}
		return StateReady, nil
	case StateTxStreaming:
		if hasMore {
			return StateTxStreaming, nil
		}
		return StateTxReady, nil
	default:
		return StateFailed, ErrInvalidTransition
	}
}
