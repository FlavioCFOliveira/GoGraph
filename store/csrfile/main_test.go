package csrfile

import (
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/subproc"
)

func TestMain(m *testing.M) {
	subproc.Dispatch()
	goleak.VerifyTestMain(m)
}
