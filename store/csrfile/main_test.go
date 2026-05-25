package csrfile

import (
	"testing"

	"go.uber.org/goleak"

	"gograph/internal/subproc"
)

func TestMain(m *testing.M) {
	subproc.Dispatch()
	goleak.VerifyTestMain(m)
}
