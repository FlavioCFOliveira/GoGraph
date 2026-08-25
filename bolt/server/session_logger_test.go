package server_test

// session_logger_test.go — Options.Logger must reach the SESSION, not only the
// accept loop (rmp #2481).
//
// The regression this guards was found by the DST auth scenario: it built its
// server with a discarding logger precisely because it provokes dozens of refused
// credentials on purpose, and the refusals still appeared on stderr. The cause was
// that `newSession` hard-coded slog.Default() and the server bootstrap never
// overrode it, so every session-level event — a refused credential, a failed
// query, a failed commit, a transaction-quota refusal — bypassed the configured
// logger. That is the majority of what a Bolt server logs, and Options.Logger
// documents itself as "the structured logger for server events".
//
// The test fails on the old behaviour: the capturing handler sees nothing.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// captureHandler is a slog.Handler that records every message it is given. It is
// safe for concurrent use because the server logs from the connection goroutines.
type captureHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

//nolint:gocritic // hugeParam: slog.Record is passed by value because slog.Handler fixes this signature; not a hot path.
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

// saw reports whether any captured message contains sub.
func (h *captureHandler) saw(sub string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

// count returns how many messages were captured.
func (h *captureHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.msgs)
}

// TestOptionsLogger_ReachesSessionEvents drives a refused credential and a failed
// query against a server whose Options.Logger is a capturing handler, and requires
// both events to arrive there.
func TestOptionsLogger_ReachesSessionEvents(t *testing.T) {
	capture := &captureHandler{}
	addr := startTestServer(t, server.Options{
		ConnTimeout: 10 * time.Second,
		Logger:      slog.New(capture),
		Auth: server.BasicAuthHandler{
			Validate: server.ConstantTimeValidate("alice", "s3cret"),
		},
	})

	// A refused credential: the session logs "authentication failed".
	bad := newBoltTestClient(t, addr)
	bad.negotiateVersion(t, 5, 6)
	bad.sendRequest(t, &proto.Hello{Extra: map[string]packstream.Value{"user_agent": "logger-test/1"}})
	bad.recvSuccess(t)
	bad.sendRequest(t, &proto.Logon{Auth: map[string]packstream.Value{
		"scheme": "basic", "principal": "alice", "credentials": "wrong",
	}})
	if f := bad.recvFailure(t); f.Code != "Neo.ClientError.Security.Unauthorized" {
		t.Fatalf("LOGON failure code %q, want Unauthorized", f.Code)
	}
	bad.close(t)

	if !capture.saw("authentication failed") {
		t.Errorf("Options.Logger did not receive the session's authentication-failure event (captured %d message(s)): Options.Logger does not reach the session", capture.count())
	}

	// A failed query: the session logs "query execution failed".
	good := newBoltTestClient(t, addr)
	good.negotiateVersion(t, 5, 6)
	good.sendRequest(t, &proto.Hello{Extra: map[string]packstream.Value{"user_agent": "logger-test/1"}})
	good.recvSuccess(t)
	good.sendRequest(t, &proto.Logon{Auth: map[string]packstream.Value{
		"scheme": "basic", "principal": "alice", "credentials": "s3cret",
	}})
	good.recvSuccess(t)
	good.sendRequest(t, &proto.Run{
		Query:      "THIS IS NOT CYPHER",
		Parameters: map[string]packstream.Value{},
		Extra:      map[string]packstream.Value{},
	})
	if f := good.recvFailure(t); f.Code == "" {
		t.Fatalf("a malformed query produced a FAILURE with no code")
	}
	good.close(t)

	if !capture.saw("query execution failed") {
		t.Errorf("Options.Logger did not receive the session's query-failure event (captured %d message(s))", capture.count())
	}
}

// TestOptionsLogger_NilKeepsDefault pins that a nil Logger still works: the
// session falls back to slog.Default() rather than logging through a nil handler
// (which would panic on the first refused credential).
func TestOptionsLogger_NilKeepsDefault(t *testing.T) {
	addr := startTestServer(t, server.Options{
		ConnTimeout: 10 * time.Second,
		Auth: server.BasicAuthHandler{
			Validate: server.ConstantTimeValidate("alice", "s3cret"),
		},
	})
	c := newBoltTestClient(t, addr)
	c.negotiateVersion(t, 5, 6)
	c.sendRequest(t, &proto.Hello{Extra: map[string]packstream.Value{"user_agent": "logger-test/2"}})
	c.recvSuccess(t)
	c.sendRequest(t, &proto.Logon{Auth: map[string]packstream.Value{
		"scheme": "basic", "principal": "alice", "credentials": "wrong",
	}})
	if f := c.recvFailure(t); f.Code != "Neo.ClientError.Security.Unauthorized" {
		t.Fatalf("LOGON failure code %q, want Unauthorized", f.Code)
	}
	c.close(t)
}
