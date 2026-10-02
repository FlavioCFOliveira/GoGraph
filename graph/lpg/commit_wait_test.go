package lpg

// commit_wait_test.go — rmp #2965 round 5, finding R5-F2: a write refused by a
// bounded (durable) commit's uncommitted version parks on that commit's end, in
// arrival order, instead of polling; and the commit releases it on every exit.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// holdDurable starts a bounded commit that writes x.p and parks in its durable
// step until release is closed, then ends as end decides. It returns once the
// commit holds its claim.
func holdDurable(t *testing.T, g *Graph[string, float64], end func() error) (release chan struct{}, done chan error) {
	t.Helper()
	inDurable := make(chan struct{})
	release = make(chan struct{})
	done = make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("panicked: %v", r)
			}
		}()
		done <- g.ApplyDurable(context.Background(), func(wtx WriteTx) error {
			return g.Writer(wtx).SetNodeProperty("x", "p", Int64Value(1))
		}, func() error {
			close(inDurable)
			<-release
			return end()
		})
	}()
	<-inDurable
	return release, done
}

func TestApplyDurable_WaiterParksUntilTheBlockerEnds(t *testing.T) {
	boom := errors.New("injected fsync failure")
	cases := []struct {
		name string
		end  func() error
	}{
		{"published", func() error { return nil }},
		{"fsync failed", func() error { return boom }},
		{"panicked", func() error { panic("injected durable panic") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newDirectTxGraph(t, true)
			requireNoErr(t, g.AddNode("x"))
			release, blockerDone := holdDurable(t, g, c.end)

			var attempts atomic.Int64
			waiterDone := make(chan error, 1)
			go func() {
				waiterDone <- g.ApplyDurable(context.Background(), func(wtx WriteTx) error {
					attempts.Add(1)
					return g.Writer(wtx).SetNodeProperty("x", "p", Int64Value(2))
				}, func() error { return nil })
			}()
			// Hold the claim long enough for a polling waiter to rerun its apply
			// many times over; a parked one runs it once.
			time.Sleep(50 * time.Millisecond)
			if n := attempts.Load(); n != 1 {
				t.Errorf("waiter ran its apply %d times while the blocker held its claim, want 1 (parked)", n)
			}
			close(release)
			<-blockerDone
			select {
			case err := <-waiterDone:
				if err != nil {
					t.Fatalf("waiter: %v", err)
				}
			case <-time.After(directWaitBudget):
				t.Fatal("waiter was not released when the blocker ended")
			}
			if n := attempts.Load(); n > 2 {
				t.Errorf("waiter ran its apply %d times, want at most 2", n)
			}
			if v, ok := g.GetNodeProperty("x", "p"); !ok || v != Int64Value(2) {
				t.Errorf("x.p = %v,%v after the waiter committed, want 2", v, ok)
			}
			if n := g.txWait.handoffs.Load(); n != 0 {
				t.Errorf("hand-off tokens outstanding = %d, want 0", n)
			}
		})
	}
}

func TestApplyDurable_WaitersAreServedInArrivalOrder(t *testing.T) {
	g := newDirectTxGraph(t, true)
	requireNoErr(t, g.AddNode("x"))
	release, blockerDone := holdDurable(t, g, func() error { return nil })

	const n = 6
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		var attempts atomic.Int64
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := g.ApplyDurable(context.Background(), func(wtx WriteTx) error {
				attempts.Add(1)
				return g.Writer(wtx).SetNodeProperty("x", "p", Int64Value(int64(i)))
			}, func() error {
				mu.Lock()
				order = append(order, i)
				mu.Unlock()
				return nil
			})
			if err != nil {
				t.Errorf("waiter %d: %v", i, err)
			}
		}()
		// Arrive strictly after the previous waiter has parked.
		for attempts.Load() == 0 {
			time.Sleep(100 * time.Microsecond)
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	<-blockerDone
	wg.Wait()
	for i, got := range order {
		if got != i {
			t.Fatalf("commit order %v, want arrival order 0..%d", order, n-1)
		}
	}
	if len(order) != n {
		t.Fatalf("%d of %d waiters committed", len(order), n)
	}
}

func TestDirectWrite_ParksOnABoundedCommit(t *testing.T) {
	g := newDirectTxGraph(t, true)
	requireNoErr(t, g.AddNode("x"))
	release, blockerDone := holdDurable(t, g, func() error { return nil })
	done := make(chan error, 1)
	go func() { done <- g.SetNodeProperty("x", "p", Int64Value(3)) }()
	time.Sleep(20 * time.Millisecond)
	close(release)
	<-blockerDone
	if err := <-done; err != nil {
		t.Fatalf("direct write: %v", err)
	}
	if v, _ := g.GetNodeProperty("x", "p"); v != Int64Value(3) {
		t.Fatalf("x.p = %v, want the direct write's 3 (it ran after the commit)", v)
	}
}
