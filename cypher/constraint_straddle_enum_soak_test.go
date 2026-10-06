package cypher

// constraint_straddle_enum_soak_test.go — the whole straddler space of
// constraint_straddle_enum_test.go, both families: every case on the in-memory
// wiring in the soak layer, and every case on the WAL-backed wiring in the
// nightly layer.

import (
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

func TestConstraintStraddle_EnumerationFull_InMemory(t *testing.T) {
	testlayers.RequireSoak(t)
	cases := append(straddleCases(false, straddleAll), straddleSeqCases(false, straddleAll)...)
	rep := runStraddleCases(t, cases, runtime.GOMAXPROCS(0), false)
	t.Logf("in-memory: %s", rep.summary())
}

func TestConstraintStraddle_EnumerationFull_WAL(t *testing.T) {
	testlayers.RequireNightly(t)
	cases := append(straddleCases(true, straddleAll), straddleSeqCases(true, straddleAll)...)
	rep := runStraddleCases(t, cases, runtime.GOMAXPROCS(0), false)
	t.Logf("WAL-backed: %s", rep.summary())
}
