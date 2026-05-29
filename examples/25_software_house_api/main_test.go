package main

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs every test under goleak so a leaked goroutine fails the
// package. The store, engine and WAL spawn no background goroutines; the
// HTTP tests close their httptest servers and disable client keep-alives,
// so a clean run leaks nothing.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
