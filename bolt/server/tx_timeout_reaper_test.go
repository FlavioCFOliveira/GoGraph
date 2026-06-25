package server_test

// tx_timeout_reaper_test.go — regression gate for task #1346 (P7/S7):
// an idle-but-open explicit transaction holds the engine's single global writer
// lock until COMMIT/ROLLBACK. The idle ConnTimeout is reset before every read,
// so a client could keep the connection alive with no-op messages (e.g. LOGON
// in TX_READY) and hold that lock far past the transaction timeout — a
// server-wide write DoS.
//
// The fix arms a reaper at the transaction's wall-clock deadline that closes the
// connection; the deferred session teardown then rolls the transaction back and
// releases the writer lock, independent of message arrival.
//
// GATE: with a short DefaultTxTimeout and a long ConnTimeout, connection A opens
// a transaction and pings LOGON forever; a second connection B's write must
// still complete promptly (the lock was released) and A must be reaped. On the
// unfixed code B blocks until A stops pinging (here: until the test's own
// teardown), so B's write times out and the test fails.
//
// Layer: short. Goroutine cleanliness is enforced by the package goleak
// TestMain; the ping goroutine is joined before the test returns.

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// pingLogon sends a single LOGON (a legal no-op in TX_READY under NoAuthHandler)
// and reads its response, returning the decoded response and the first error
// encountered. It is used by the keep-alive goroutine and never calls t.Fatal
// (it runs off the test goroutine). After the transaction is reaped the session
// is FAILED, so the LOGON response becomes a *proto.Failure (the gentle reap) or
// the connection errors (a hard reap) — either signals the reap.
func (c *boltTestClient) pingLogon() (any, error) {
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := proto.EncodeRequest(enc, &proto.Logon{Auth: map[string]packstream.Value{
		"scheme":      "none",
		"principal":   "test",
		"credentials": "",
	}}); err != nil {
		return nil, err
	}
	if err := enc.Flush(); err != nil {
		return nil, err
	}
	if err := c.cw.WriteMessage(buf.Bytes()); err != nil {
		return nil, err
	}
	raw, err := c.cr.ReadMessage()
	if err != nil {
		return nil, err
	}
	dec := packstream.NewDecoder(bytes.NewReader(raw))
	return proto.DecodeResponse(dec)
}

// TestTxTimeout_IdleOpenTransactionIsReaped is the regression gate for #1346.
func TestTxTimeout_IdleOpenTransactionIsReaped(t *testing.T) {
	t.Parallel()

	const (
		txTimeout   = 300 * time.Millisecond
		connTimeout = 10 * time.Second // long, so idle close never masks the reaper
		// Generous upper bound: the reaper should fire at ~txTimeout; anything
		// well under connTimeout proves the timeout (not the idle close) reaped it.
		reapBudget = 4 * time.Second
	)

	addr := startTestServer(t, server.Options{
		DefaultTxTimeout: txTimeout,
		ConnTimeout:      connTimeout,
	})

	// Connection A: open an explicit transaction (acquires the engine's writer
	// lock) and keep the connection alive with LOGON pings.
	a := newBoltTestClient(t, addr)
	defer a.close(t)
	a.negotiate(t)
	a.hello(t)
	a.begin(t)
	txOpened := time.Now()

	aReaped := make(chan time.Duration, 1)
	stopPing := make(chan struct{})
	var pingWG sync.WaitGroup
	pingWG.Add(1)
	go func() {
		defer pingWG.Done()
		for {
			select {
			case <-stopPing:
				return
			default:
			}
			resp, err := a.pingLogon()
			if err != nil {
				// Hard reap: the server closed the connection.
				select {
				case aReaped <- time.Since(txOpened):
				default:
				}
				return
			}
			// Gentle reap: the transaction was rolled back and the session moved
			// to FAILED. In FAILED, a non-RESET/GOODBYE message (here the no-op
			// LOGON ping) is answered with IGNORED per the Bolt v5 spec (#1781);
			// older builds replied FAILURE. Either response signals the reap.
			switch resp.(type) {
			case *proto.Failure, *proto.Ignored:
				select {
				case aReaped <- time.Since(txOpened):
				default:
				}
				return
			}
			time.Sleep(txTimeout / 4) // ping well under ConnTimeout
		}
	}()
	defer func() {
		close(stopPing)
		pingWG.Wait()
	}()

	// Connection B: a second client whose autocommit write needs the same writer
	// lock A holds. With the fix it completes once A's transaction is reaped.
	b := newBoltTestClient(t, addr)
	defer b.close(t)
	b.negotiate(t)
	b.hello(t)

	writeStart := time.Now()
	b.run(t, "CREATE (:Reaped)", nil)
	b.pullAll(t)
	writeElapsed := time.Since(writeStart)

	if writeElapsed > reapBudget {
		t.Fatalf("connection B's write blocked %v on connection A's idle-but-open transaction; "+
			"the transaction timeout did not release the writer lock (DoS)", writeElapsed)
	}

	// Confirm connection A was reaped (its connection closed by the timeout
	// reaper) rather than lingering until the idle ConnTimeout.
	select {
	case d := <-aReaped:
		if d > reapBudget {
			t.Fatalf("connection A reaped after %v, want within %v of BEGIN", d, reapBudget)
		}
		t.Logf("connection A reaped %v after BEGIN; B's write completed in %v", d, writeElapsed)
	case <-time.After(reapBudget):
		t.Fatal("connection A's idle-but-open transaction was never reaped (connection still alive past the budget)")
	}
}
