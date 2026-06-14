package server

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

func TestTransition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		current   State
		msg       any
		success   bool
		wantState State
		wantErr   bool
	}{
		// ── Normal happy path ────────────────────────────────────────────────
		{
			name:      "NEGOTIATION+Hello+ok→READY",
			current:   StateNegotiation,
			msg:       &proto.Hello{},
			success:   true,
			wantState: StateReady,
		},
		{
			name:      "READY+Run+ok→STREAMING",
			current:   StateReady,
			msg:       &proto.Run{},
			success:   true,
			wantState: StateStreaming,
		},
		{
			name:      "STREAMING+Pull+ok→READY",
			current:   StateStreaming,
			msg:       &proto.Pull{},
			success:   true,
			wantState: StateReady,
		},
		{
			name:      "STREAMING+Discard+ok→READY",
			current:   StateStreaming,
			msg:       &proto.Discard{},
			success:   true,
			wantState: StateReady,
		},
		{
			name:      "READY+Begin+ok→TX_READY",
			current:   StateReady,
			msg:       &proto.Begin{},
			success:   true,
			wantState: StateTxReady,
		},
		{
			name:      "TX_READY+Run+ok→TX_STREAMING",
			current:   StateTxReady,
			msg:       &proto.Run{},
			success:   true,
			wantState: StateTxStreaming,
		},
		{
			name:      "TX_STREAMING+Pull+ok→TX_READY",
			current:   StateTxStreaming,
			msg:       &proto.Pull{},
			success:   true,
			wantState: StateTxReady,
		},
		{
			name:      "TX_READY+Commit+ok→READY",
			current:   StateTxReady,
			msg:       &proto.Commit{},
			success:   true,
			wantState: StateReady,
		},
		{
			name:      "TX_READY+Rollback+ok→READY",
			current:   StateTxReady,
			msg:       &proto.Rollback{},
			success:   true,
			wantState: StateReady,
		},
		// ── RESET from various states ────────────────────────────────────────
		{
			name:      "STREAMING+Reset→READY",
			current:   StateStreaming,
			msg:       &proto.Reset{},
			success:   true,
			wantState: StateReady,
		},
		{
			name:      "FAILED+Reset→READY",
			current:   StateFailed,
			msg:       &proto.Reset{},
			success:   true,
			wantState: StateReady,
		},
		// Pre-authentication RESET must not mint READY: it returns to NEGOTIATION
		// so a HELLO is still required (task #1345).
		{
			name:      "NEGOTIATION+Reset→NEGOTIATION",
			current:   StateNegotiation,
			msg:       &proto.Reset{},
			success:   true,
			wantState: StateNegotiation,
		},
		{
			name:      "CONNECTED+Reset→NEGOTIATION",
			current:   StateConnected,
			msg:       &proto.Reset{},
			success:   true,
			wantState: StateNegotiation,
		},
		// ── GOODBYE from various states ──────────────────────────────────────
		{
			name:      "READY+Goodbye→DEFUNCT",
			current:   StateReady,
			msg:       &proto.Goodbye{},
			success:   true,
			wantState: StateDefunct,
		},
		{
			name:      "STREAMING+Goodbye→DEFUNCT",
			current:   StateStreaming,
			msg:       &proto.Goodbye{},
			success:   true,
			wantState: StateDefunct,
		},
		// ── Error paths ──────────────────────────────────────────────────────
		{
			name:      "NEGOTIATION+Hello+fail→FAILED",
			current:   StateNegotiation,
			msg:       &proto.Hello{},
			success:   false,
			wantState: StateFailed,
		},
		{
			name:      "READY+Run+fail→FAILED",
			current:   StateReady,
			msg:       &proto.Run{},
			success:   false,
			wantState: StateFailed,
		},
		{
			name:      "CONNECTED+Run→invalid",
			current:   StateConnected,
			msg:       &proto.Run{},
			success:   true,
			wantState: StateFailed,
			wantErr:   true,
		},
		{
			name:      "FAILED+Run→invalid",
			current:   StateFailed,
			msg:       &proto.Run{},
			success:   true,
			wantState: StateFailed,
			wantErr:   true,
		},
		{
			name:      "DEFUNCT+Reset→invalid",
			current:   StateDefunct,
			msg:       &proto.Reset{},
			success:   true,
			wantState: StateDefunct,
			wantErr:   true,
		},
		{
			name:      "READY+Pull→invalid",
			current:   StateReady,
			msg:       &proto.Pull{},
			success:   true,
			wantState: StateFailed,
			wantErr:   true,
		},
		// ── StateAuthentication (Bolt >= 5.1 pre-LOGON) (task #1470) ──────────
		{
			name:      "AUTHENTICATION+Logon+ok→READY",
			current:   StateAuthentication,
			msg:       &proto.Logon{},
			success:   true,
			wantState: StateReady,
		},
		{
			name:      "AUTHENTICATION+Logon+fail→FAILED",
			current:   StateAuthentication,
			msg:       &proto.Logon{},
			success:   false,
			wantState: StateFailed,
		},
		{
			name:      "AUTHENTICATION+Logoff+ok→AUTHENTICATION",
			current:   StateAuthentication,
			msg:       &proto.Logoff{},
			success:   true,
			wantState: StateAuthentication,
		},
		{
			name:      "AUTHENTICATION+Run→invalid",
			current:   StateAuthentication,
			msg:       &proto.Run{},
			success:   true,
			wantState: StateFailed,
			wantErr:   true,
		},
		{
			name:      "AUTHENTICATION+Begin→invalid",
			current:   StateAuthentication,
			msg:       &proto.Begin{},
			success:   true,
			wantState: StateFailed,
			wantErr:   true,
		},
		{
			name:      "AUTHENTICATION+Goodbye→DEFUNCT",
			current:   StateAuthentication,
			msg:       &proto.Goodbye{},
			success:   true,
			wantState: StateDefunct,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Transition(tc.current, tc.msg, tc.success)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Transition(%v, %T, %v): err=%v, wantErr=%v", tc.current, tc.msg, tc.success, err, tc.wantErr)
			}
			if got != tc.wantState {
				t.Fatalf("Transition(%v, %T, %v): got %v, want %v", tc.current, tc.msg, tc.success, got, tc.wantState)
			}
		})
	}
}

func TestStreamingTransition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		current   State
		hasMore   bool
		wantState State
		wantErr   bool
	}{
		{"STREAMING+hasMore→STREAMING", StateStreaming, true, StateStreaming, false},
		{"STREAMING+done→READY", StateStreaming, false, StateReady, false},
		{"TX_STREAMING+hasMore→TX_STREAMING", StateTxStreaming, true, StateTxStreaming, false},
		{"TX_STREAMING+done→TX_READY", StateTxStreaming, false, StateTxReady, false},
		{"READY+hasMore→invalid", StateReady, true, StateFailed, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := StreamingTransition(tc.current, tc.hasMore)
			if (err != nil) != tc.wantErr {
				t.Fatalf("StreamingTransition(%v, %v): err=%v, wantErr=%v", tc.current, tc.hasMore, err, tc.wantErr)
			}
			if got != tc.wantState {
				t.Fatalf("StreamingTransition(%v, %v): got %v, want %v", tc.current, tc.hasMore, got, tc.wantState)
			}
		})
	}
}

// TestHelloTransition covers the version-aware HELLO transition added for the
// Bolt >= 5.1 deferred-authentication flow (task #1470): a successful HELLO
// routes to StateAuthentication on >= 5.1 and to StateReady on <= 5.0 (including
// the zero-value version used by direct white-box tests); a failed or
// out-of-state HELLO delegates to Transition.
func TestHelloTransition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		current   State
		ver       proto.Version
		success   bool
		wantState State
		wantErr   bool
	}{
		{"NEGOTIATION+5.1+ok→AUTHENTICATION", StateNegotiation, proto.Version{Major: 5, Minor: 1}, true, StateAuthentication, false},
		{"NEGOTIATION+5.6+ok→AUTHENTICATION", StateNegotiation, proto.Version{Major: 5, Minor: 6}, true, StateAuthentication, false},
		{"NEGOTIATION+5.0+ok→READY", StateNegotiation, proto.Version{Major: 5, Minor: 0}, true, StateReady, false},
		{"NEGOTIATION+4.4+ok→READY", StateNegotiation, proto.Version{Major: 4, Minor: 4}, true, StateReady, false},
		{"NEGOTIATION+zero+ok→READY", StateNegotiation, proto.Version{}, true, StateReady, false},
		{"NEGOTIATION+5.1+fail→FAILED", StateNegotiation, proto.Version{Major: 5, Minor: 1}, false, StateFailed, false},
		// Out of state: delegates to Transition, which reports an illegal transition.
		{"READY+5.1+ok→invalid", StateReady, proto.Version{Major: 5, Minor: 1}, true, StateFailed, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := HelloTransition(tc.current, tc.ver, tc.success)
			if (err != nil) != tc.wantErr {
				t.Fatalf("HelloTransition(%v, %v, %v): err=%v, wantErr=%v", tc.current, tc.ver, tc.success, err, tc.wantErr)
			}
			if got != tc.wantState {
				t.Fatalf("HelloTransition(%v, %v, %v): got %v, want %v", tc.current, tc.ver, tc.success, got, tc.wantState)
			}
		})
	}
}

func TestStateString(t *testing.T) {
	t.Parallel()
	states := []struct {
		s    State
		want string
	}{
		{StateConnected, "CONNECTED"},
		{StateNegotiation, "NEGOTIATION"},
		{StateAuthentication, "AUTHENTICATION"},
		{StateReady, "READY"},
		{StateStreaming, "STREAMING"},
		{StateTxReady, "TX_READY"},
		{StateTxStreaming, "TX_STREAMING"},
		{StateFailed, "FAILED"},
		{StateDefunct, "DEFUNCT"},
	}
	for _, tc := range states {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := tc.s.String(); got != tc.want {
				t.Fatalf("State(%d).String() = %q, want %q", tc.s, got, tc.want)
			}
		})
	}
}
