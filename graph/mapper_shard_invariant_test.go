package graph

import (
	"fmt"
	"math/rand/v2"
	"testing"
)

// TestMapper_MaxNodeID_ShardByte verifies that the shard component of
// MaxNodeID is within the valid shard range [0, 255].
func TestMapper_MaxNodeID_ShardByte(t *testing.T) {
	t.Parallel()
	m := NewMapper[string]()
	const n = 1000
	for i := 0; i < n; i++ {
		m.Intern(fmt.Sprintf("key-%d", i))
	}
	maxID := m.MaxNodeID()
	shard := MapperShardOf(maxID)
	if shard > 255 {
		t.Errorf("MaxNodeID shard byte = %d, want <= 255", shard)
	}
	t.Logf("MaxNodeID shard = %d", shard)
}

// TestMapper_NodeID_ShardByte_AllInRange walks every interned NodeID and
// asserts that the shard component is in [0, 255].
func TestMapper_NodeID_ShardByte_AllInRange(t *testing.T) {
	t.Parallel()
	m := NewMapper[string]()
	const n = 10_000
	for i := 0; i < n; i++ {
		m.Intern(fmt.Sprintf("key-%d", i))
	}
	m.Walk(func(id NodeID, _ string) bool {
		shard := MapperShardOf(id)
		if shard > 255 {
			t.Errorf("NodeID %d has shard byte %d, want [0,255]", id, shard)
		}
		return true
	})
}

// TestMapper_ShardDistribution_Uniform interns 100 000 random string keys
// and asserts the shard distribution passes a chi-squared goodness-of-fit
// test at the p=0.05 significance level (critical value ≈ 293.2 for 255
// degrees of freedom).
func TestMapper_ShardDistribution_Uniform(t *testing.T) {
	t.Parallel()
	m := NewMapper[string]()
	const n = 100_000
	rng := rand.New(rand.NewPCG(0xDEAD, 0xBEEF))
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	for i := 0; i < n; i++ {
		length := 8 + rng.IntN(8)
		buf := make([]byte, length)
		for j := range buf {
			buf[j] = letters[rng.IntN(len(letters))]
		}
		m.Intern(string(buf))
	}

	counts := make([]int, 256)
	m.Walk(func(id NodeID, _ string) bool {
		counts[MapperShardOf(id)]++
		return true
	})

	// Chi-squared goodness-of-fit with uniform null hypothesis.
	// Degrees of freedom = 256 - 1 = 255; critical value at p=0.05 ≈ 293.2.
	expected := float64(m.Len()) / 256.0
	var chi2 float64
	for _, c := range counts {
		diff := float64(c) - expected
		chi2 += diff * diff / expected
	}

	const criticalValue = 293.2
	if chi2 > criticalValue {
		t.Errorf("chi-squared = %.2f > %.2f: shard distribution is non-uniform", chi2, criticalValue)
	}
	t.Logf("chi-squared = %.2f (critical=%.2f, n=%d)", chi2, criticalValue, m.Len())
}
