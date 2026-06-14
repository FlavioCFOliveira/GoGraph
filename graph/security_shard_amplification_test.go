package graph

import (
	"strconv"
	"testing"
)

// security_shard_amplification_test.go is part of the GoGraph security
// test battery. It documents the NodeID-space amplification of [Mapper]
// under an adversarial single-shard key flood (rmp #1474) and pins the
// FIX strategy that was chosen for it.
//
// A Mapper packs NodeID = (intraShardIndex << 8) | shard, with 256
// shards. MaxNodeID() is computed as packNodeID(255, maxIntra-1)+1 where
// maxIntra is the largest per-shard reverse-slice length. When every key
// collides on a single shard, that shard holds all N entries, so:
//
//	MaxNodeID() == N*256   and   Order()/Len() == N
//	=> MaxNodeID()/Order() == 256 == MapperShardCount()
//
// i.e. an attacker who controls natural keys can inflate the dense
// NodeID range by exactly the shard count relative to the real node
// count.
//
// FIX CHOSEN (#1474): the structural amplification of the Mapper itself
// is LEFT IN PLACE on purpose. The shard hash (FNV-1a) is deliberately
// unseeded and stable across processes because that stability is a
// durability requirement: a snapshot written by one process and
// reopened by another must agree on the same NodeID for the same
// natural key, and [Mapper.LoadFrom] re-validates each persisted NodeID
// against mapperShardFor(key) on reload. Randomising a per-Mapper shard
// seed would change NodeIDs for existing persisted data and break ACID
// durability — see TestSec_Core_MapperReloadIDStability below, which
// proves the persist→reload contract the seed-fix would have violated.
//
// Instead the fix is applied at the ANALYTICS layer: the uncompacted
// search algorithms that sized buffers on MaxNodeID() (search.TC,
// search.WCC) now compact through CSR.LiveMask() into a dense
// [0, live) index space, so their allocation is O(live) / O(live^2)
// over the real node count rather than O(MaxNodeID) / O(MaxNodeID^2).
// The structural factor pinned here is therefore the INPUT to the
// fixed envelope, not a bug in the Mapper. The analytics envelope is
// pinned in search/security_shardflood_envelope_soak_test.go.
//
// This file lives in package graph (white-box) and derives its own
// shard-0 keys rather than importing internal/shapegen, which would
// create a graph -> shapegen -> graph import cycle.

// secDeriveShardZeroKeys brute-forces n distinct string keys that all
// route to mapper shard 0 under the FNV-1a scheme. Acceptance is
// 1/MapperShardCount per candidate, so n in the low thousands is cheap
// (well under a few hundred ms). It mirrors shapegen.computeShardZeroKeys
// but stays inside package graph to avoid the import cycle.
func secDeriveShardZeroKeys(tb testing.TB, n int) []string {
	tb.Helper()
	m := NewMapper[string]()
	keys := make([]string, 0, n)
	buf := make([]byte, 0, 16)
	maxCandidates := 1000*n + 1_000_000
	for i := 0; len(keys) < n; i++ {
		if i > maxCandidates {
			tb.Fatalf("secDeriveShardZeroKeys: exhausted %d candidates collecting %d keys (got %d)",
				i, n, len(keys))
		}
		buf = buf[:0]
		buf = append(buf, 's', 'z', '-')
		buf = strconv.AppendInt(buf, int64(i), 10)
		s := string(buf)
		if MapperShardOf(m.Intern(s)) == 0 {
			keys = append(keys, s)
		}
	}
	return keys
}

// TestSec_Core_MapperShardAmplificationFactor documents that a
// worst-case single-shard key flood inflates MaxNodeID() to exactly
// MapperShardCount() times Order(). The magnitude is kept small (a few
// thousand keys) because the amplification factor is independent of N —
// proving it on 4096 keys proves it for any N.
//
// SECURITY-GAP #1474 (mitigated at the analytics layer): the FNV-1a
// shard hash is unseeded and stable across processes, so an attacker who
// controls natural keys can force every key onto one shard and drive
// MaxNodeID()/Order() up to the shard count (256). This factor is left
// in place by design — it is a durability requirement, see the file
// header and TestSec_Core_MapperReloadIDStability — and the downstream
// over-allocation is removed instead by LiveMask compaction in
// search.TC / search.WCC (envelope pinned in the search soak test). This
// test asserts the factor remains EXACTLY the shard count so that the
// analytics envelope continues to be computed against the correct input.
func TestSec_Core_MapperShardAmplificationFactor(t *testing.T) {
	t.Parallel()

	const nKeys = 4096
	keys := secDeriveShardZeroKeys(t, nKeys)

	m := NewMapper[string]()
	for _, k := range keys {
		m.Intern(k)
	}

	order := m.Len()
	if order != nKeys {
		t.Fatalf("Len() = %d, want %d distinct keys interned", order, nKeys)
	}
	maxID := uint64(m.MaxNodeID())

	// Every key must have landed on shard 0 (precondition for the flood).
	var offShard int
	m.Walk(func(id NodeID, _ string) bool {
		if MapperShardOf(id) != 0 {
			offShard++
		}
		return true
	})
	if offShard != 0 {
		t.Fatalf("%d keys did not land on shard 0; flood precondition broken", offShard)
	}

	// The amplification factor equals the shard count exactly.
	wantMaxID := uint64(order) * uint64(MapperShardCount())
	if maxID != wantMaxID {
		t.Fatalf("MaxNodeID() = %d, want Order()*ShardCount() = %d*%d = %d",
			maxID, order, MapperShardCount(), wantMaxID)
	}

	factor := maxID / uint64(order)
	if factor != uint64(MapperShardCount()) {
		t.Fatalf("amplification factor MaxNodeID()/Order() = %d, want %d (the shard count)",
			factor, MapperShardCount())
	}
	t.Logf("shard-0 flood: Order()=%d MaxNodeID()=%d amplification=%dx (== MapperShardCount %d); "+
		"mitigated downstream by LiveMask compaction, not by reseeding (see TestSec_Core_MapperReloadIDStability)",
		order, maxID, factor, MapperShardCount())
}

// TestSec_Core_MapperReloadIDStability is the persistence-safety proof
// that justifies fixing #1474 at the analytics layer rather than by
// randomising the per-Mapper shard seed.
//
// It exercises the exact serialise→reload contract the snapshot layer
// uses: enumerate every (NodeID, key) pair with [Mapper.Walk] (what the
// snapshot writer does), then rebuild a FRESH mapper from those pairs
// with [Mapper.LoadFrom] (what recovery does). It then asserts that
// every key resolves to the IDENTICAL NodeID after the round-trip, for
// both an ordinary key set and an adversarial shard-0-flooded one.
//
// This is the property a randomised shard seed would break:
// [Mapper.LoadFrom] re-validates each persisted NodeID against
// mapperShardFor(key) (precondition 2), so a seed change between write
// and reload would either reject the snapshot as corrupted or remap
// NodeIDs — a durability/ACID violation. A green run here proves the
// shard hash is deterministic and seed-free, so the persisted NodeID↔key
// mapping survives a reload unchanged.
func TestSec_Core_MapperReloadIDStability(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		keys []string
	}{
		{
			name: "ordinary_keys",
			keys: func() []string {
				ks := make([]string, 0, 500)
				for i := 0; i < 500; i++ {
					ks = append(ks, "user-"+strconv.Itoa(i))
				}
				return ks
			}(),
		},
		{
			name: "adversarial_shard0_flood",
			keys: secDeriveShardZeroKeys(t, 1024),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Original interning.
			src := NewMapper[string]()
			want := make(map[string]NodeID, len(tc.keys))
			for _, k := range tc.keys {
				want[k] = src.Intern(k)
			}

			// "Persist": enumerate pairs in the same Walk order the
			// snapshot writer serialises.
			entries := make([]MapperEntry[string], 0, src.Len())
			src.Walk(func(id NodeID, k string) bool {
				entries = append(entries, MapperEntry[string]{ID: id, Key: k})
				return true
			})

			// "Reload": rebuild a fresh mapper from the persisted pairs.
			// LoadFrom re-validates each NodeID's shard against
			// mapperShardFor(key); success alone proves the hash is
			// deterministic and seed-free.
			dst := NewMapper[string]()
			if err := dst.LoadFrom(entries); err != nil {
				t.Fatalf("LoadFrom after persist: %v (a non-deterministic shard "+
					"hash would surface here as ErrMapperEntryCorrupted)", err)
			}

			if got, w := dst.Len(), src.Len(); got != w {
				t.Fatalf("reloaded Len() = %d, want %d", got, w)
			}
			if got, w := uint64(dst.MaxNodeID()), uint64(src.MaxNodeID()); got != w {
				t.Fatalf("reloaded MaxNodeID() = %d, want %d", got, w)
			}

			// Every key must resolve to the IDENTICAL NodeID, and the
			// reverse resolution must round-trip too.
			for k, id := range want {
				got, ok := dst.Lookup(k)
				if !ok {
					t.Fatalf("reloaded Lookup(%q) not found", k)
				}
				if got != id {
					t.Fatalf("reloaded Lookup(%q) = %d, want %d (NodeID NOT stable across reload)",
						k, got, id)
				}
				if rk, ok := dst.Resolve(id); !ok || rk != k {
					t.Fatalf("reloaded Resolve(%d) = (%q,%v), want (%q,true)", id, rk, ok, k)
				}
			}

			// A freshly re-interned mapper over the same keys (a brand
			// new process re-ingesting the same input) must produce the
			// IDENTICAL NodeIDs — the cross-process determinism the FNV
			// hash guarantees and on which snapshot/recovery depends.
			reintern := NewMapper[string]()
			for _, k := range tc.keys {
				if got, w := reintern.Intern(k), want[k]; got != w {
					t.Fatalf("re-intern Intern(%q) = %d, want %d (shard hash not deterministic)",
						k, got, w)
				}
			}
		})
	}
}

// TestSec_Core_MapperUniformDistributionNoAmplification is the DEFENSE
// counterpart: a uniformly distributed key set keeps MaxNodeID() close
// to Order() (amplification well below the shard count). This pins the
// well-behaved baseline so a regression that makes uniform keys also
// amplify would be caught here, and documents that #1474 is specifically
// an adversarial-key concern, not a property of normal workloads.
func TestSec_Core_MapperUniformDistributionNoAmplification(t *testing.T) {
	t.Parallel()

	m := NewMapper[string]()
	const nKeys = 100_000
	for i := 0; i < nKeys; i++ {
		m.Intern("uniform-key-" + strconv.Itoa(i))
	}
	order := m.Len()
	maxID := uint64(m.MaxNodeID())

	// With ~uniform spread over 256 shards, the busiest shard holds
	// roughly Order()/256 entries, so MaxNodeID() ≈ Order() and the
	// amplification stays far below the shard count. We allow generous
	// slack (factor < 2) to keep the test robust to hash skew while still
	// proving it is nowhere near the 256× adversarial worst case.
	factor := float64(maxID) / float64(order)
	if factor >= 2.0 {
		t.Fatalf("uniform keys amplified by %.2fx (MaxNodeID=%d Order=%d); expected < 2x",
			factor, maxID, order)
	}
	t.Logf("uniform keys: Order()=%d MaxNodeID()=%d amplification=%.3fx (worst-case adversarial is %dx)",
		order, maxID, factor, MapperShardCount())
}
