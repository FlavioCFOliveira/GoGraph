package server

import (
	"testing"

	"gograph/bolt/proto"
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

func TestStateString(t *testing.T) {
	t.Parallel()
	states := []struct {
		s    State
		want string
	}{
		{StateConnected, "CONNECTED"},
		{StateNegotiation, "NEGOTIATION"},
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
