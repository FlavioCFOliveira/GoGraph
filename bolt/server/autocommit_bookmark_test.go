package server

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// terminalMeta runs a statement to its terminal PULL SUCCESS and returns that
// SUCCESS's metadata.
//
// QID is -1, not the zero value: the server reads any QID >= 0 as naming a
// specific query and answers "no such query: qid 0" for a stream it never
// registered under that id, so a PULL built with an unset QID is REFUSED. The
// first version of this test used the zero value and reported the failure as
// though the RUN had been rejected.
func terminalMeta(t *testing.T, s *Session, query string) map[string]any {
	t.Helper()
	ctx := context.Background()
	if msgs, err := s.HandleMessage(ctx, &proto.Run{Query: query, Extra: map[string]interface{}{}}); err != nil {
		t.Fatalf("RUN %q: %v", query, err)
	} else if f := failureOf(msgs); f != nil {
		t.Fatalf("RUN %q refused: %s: %s", query, f.Code, f.Message)
	}
	msgs, err := s.HandleMessage(ctx, &proto.Pull{N: -1, QID: -1})
	if err != nil {
		t.Fatalf("PULL: %v", err)
	}
	for _, m := range msgs {
		if suc, ok := m.(*proto.Success); ok {
			if _, hasMore := suc.Metadata["has_more"]; hasMore {
				if more, _ := suc.Metadata["has_more"].(bool); more {
					continue
				}
			}
			out := make(map[string]any, len(suc.Metadata))
			for k, v := range suc.Metadata {
				out[k] = v
			}
			return out
		}
	}
	t.Fatalf("no terminal SUCCESS in %#v", msgs)
	return nil
}

// bookmarkOf returns the terminal SUCCESS's bookmark and whether the field was
// present at all. The distinction matters: the specification puts the field under
// "Autocommit Transaction only", so ABSENT and EMPTY are different answers.
func bookmarkOf(meta map[string]any) (string, bool) {
	v, ok := meta["bookmark"]
	if !ok {
		return "", false
	}
	s, _ := v.(string)
	return s, true
}

// TestAutocommit_TerminalSuccessCarriesAFreshBookmark guards rmp #2563,
// reproduction shape 1: the fresh-session autocommit write.
//
// Session.bookmark was assigned in exactly ONE place, handleCommit, while both
// terminal SUCCESS sites published it unconditionally — so an autocommit
// statement reported the EMPTY string, having minted nothing.
//
// The Bolt specification describes this field as "the bookmark after committing
// this transaction (Autocommit Transaction only)", so it is precisely the
// autocommit case that must carry one.
func TestAutocommit_TerminalSuccessCarriesAFreshBookmark(t *testing.T) {
	t.Parallel()
	s := newReadySession(t)

	meta := terminalMeta(t, s, "CREATE (:N {v:1})")
	bm, present := bookmarkOf(meta)
	if !present {
		t.Fatalf("the autocommit terminal SUCCESS carries no `bookmark` field at all: %#v", meta)
	}
	if bm == "" {
		t.Errorf("the autocommit terminal SUCCESS carries an EMPTY bookmark. Session.bookmark "+
			"was only ever assigned by handleCommit, so an autocommit statement minted nothing "+
			"(rmp #2563). meta = %#v", meta)
	}
}

// TestAutocommit_BookmarkIsNotTheEarlierTransactionsToken is reproduction shape
// 2, and the worse of the two.
//
// After an explicit BEGIN/RUN/COMMIT on the same session, the autocommit terminal
// SUCCESS carried the EARLIER transaction's bookmark VERBATIM. That is strictly
// worse than an empty one: a driver cannot tell it is stale, so it treats it as
// naming the autocommit write and chains causality on work it does not describe.
func TestAutocommit_BookmarkIsNotTheEarlierTransactionsToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newReadySession(t)

	// An explicit transaction first, so a bookmark exists to be leaked.
	if _, err := s.HandleMessage(ctx, &proto.Begin{Extra: map[string]interface{}{}}); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if _, err := s.HandleMessage(ctx, &proto.Run{Query: "CREATE (:Explicit)", Extra: map[string]interface{}{}}); err != nil {
		t.Fatalf("RUN in tx: %v", err)
	}
	if _, err := s.HandleMessage(ctx, &proto.Pull{N: -1, QID: -1}); err != nil {
		t.Fatalf("PULL in tx: %v", err)
	}
	commitMsgs, err := s.HandleMessage(ctx, &proto.Commit{})
	if err != nil {
		t.Fatalf("COMMIT: %v", err)
	}
	var committed string
	for _, m := range commitMsgs {
		if suc, ok := m.(*proto.Success); ok {
			if v, ok := suc.Metadata["bookmark"].(string); ok {
				committed = v
			}
		}
	}
	if committed == "" {
		t.Fatalf("the explicit COMMIT carried no bookmark, so this test has nothing to compare "+
			"against: %#v", commitMsgs)
	}

	// Now the autocommit statement on the same session.
	meta := terminalMeta(t, s, "CREATE (:Auto)")
	bm, present := bookmarkOf(meta)
	if !present {
		t.Fatalf("the autocommit terminal SUCCESS carries no `bookmark` field: %#v", meta)
	}
	if bm == committed {
		t.Errorf("the autocommit terminal SUCCESS repeated the EARLIER transaction's bookmark "+
			"%q verbatim. A driver cannot tell it is stale and will treat it as naming the "+
			"autocommit write (rmp #2563)", bm)
	}
	if bm == "" {
		t.Errorf("the autocommit terminal SUCCESS carries an empty bookmark")
	}
}

// TestExplicitTx_TerminalPullOmitsTheBookmark pins the other half of the
// contract, and it is what stops the fix from being "mint one everywhere".
//
// The specification puts the terminal SUCCESS's bookmark under "Autocommit
// Transaction only": inside an explicit transaction the bookmark belongs on the
// COMMIT SUCCESS. Emitting it on the stream's terminal SUCCESS as well is exactly
// where the stale token came from, so absence here is the fix, not an oversight.
func TestExplicitTx_TerminalPullOmitsTheBookmark(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newReadySession(t)

	if _, err := s.HandleMessage(ctx, &proto.Begin{Extra: map[string]interface{}{}}); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if _, err := s.HandleMessage(ctx, &proto.Run{Query: "CREATE (:InTx)", Extra: map[string]interface{}{}}); err != nil {
		t.Fatalf("RUN in tx: %v", err)
	}
	msgs, err := s.HandleMessage(ctx, &proto.Pull{N: -1, QID: -1})
	if err != nil {
		t.Fatalf("PULL in tx: %v", err)
	}
	for _, m := range msgs {
		suc, ok := m.(*proto.Success)
		if !ok {
			continue
		}
		if more, _ := suc.Metadata["has_more"].(bool); more {
			continue
		}
		if _, present := suc.Metadata["bookmark"]; present {
			t.Errorf("the terminal PULL SUCCESS inside an EXPLICIT transaction carries a "+
				"`bookmark` field. The specification puts it under \"Autocommit Transaction "+
				"only\", and emitting it here is what leaked a stale token (rmp #2563). "+
				"meta = %#v", suc.Metadata)
		}
	}

	// The COMMIT, however, must still carry one.
	commitMsgs, err := s.HandleMessage(ctx, &proto.Commit{})
	if err != nil {
		t.Fatalf("COMMIT: %v", err)
	}
	var found bool
	for _, m := range commitMsgs {
		if suc, ok := m.(*proto.Success); ok {
			if v, ok := suc.Metadata["bookmark"].(string); ok && v != "" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("the explicit COMMIT SUCCESS carries no bookmark: %#v", commitMsgs)
	}
}

// TestAutocommit_EachStatementGetsItsOwnBookmark stops the fix from being
// satisfied by minting once and reusing it: two successive autocommit statements
// must report DIFFERENT tokens, or the second names the first's work.
func TestAutocommit_EachStatementGetsItsOwnBookmark(t *testing.T) {
	t.Parallel()
	s := newReadySession(t)

	first, _ := bookmarkOf(terminalMeta(t, s, "CREATE (:A)"))
	second, _ := bookmarkOf(terminalMeta(t, s, "CREATE (:B)"))
	if first == "" || second == "" {
		t.Fatalf("an autocommit statement reported an empty bookmark (%q, %q)", first, second)
	}
	if first == second {
		t.Errorf("two successive autocommit statements reported the SAME bookmark %q: the "+
			"second names the first's work", first)
	}
}
