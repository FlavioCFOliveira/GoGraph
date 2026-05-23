// Package testlayers gates tests by execution layer.
//
// GoGraph organises its test corpus into three layers:
//
//   - short — runs on every PR; default `go test ./...` with no tags.
//     Each package must finish in well under one minute.
//   - soak — minutes-long mixed workloads. Enabled by the `soak` build
//     tag, or at runtime by setting `SOAK_FULL=1` (legacy env var
//     preserved for backward compatibility). The pre-existing `stress`
//     and `soakfull` build tags are considered part of the soak family.
//   - nightly — hours-long, runner-expensive scenarios. Enabled by the
//     `nightly` build tag, or at runtime by setting `GOGRAPH_NIGHTLY=1`.
//
// Tests that cannot be split into a separate file with a build-tag
// header (for example, a single-file scenario that mixes short
// assertions with optional soak-only steps) should call
// [RequireSoak] or [RequireNightly] at the top of the test body to
// skip cleanly when the corresponding layer is inactive. New tests
// should prefer compile-time gating (build tags) over runtime
// skipping wherever practical: a build-tag-gated file simply does
// not compile out of layer, which is both faster and safer than a
// runtime skip.
//
// Sample invocations:
//
//	go test ./...                   # short layer only
//	go test -tags=soak ./...        # short + soak
//	go test -tags=nightly ./...     # short + soak + nightly
//	SOAK_FULL=1 go test ./...       # short + soak (env opt-in)
//	GOGRAPH_NIGHTLY=1 go test ./... # short + soak + nightly (env opt-in)
//
// The full layer specification lives in docs/test-layers.md.
package testlayers

import (
	"os"
	"testing"
)

// soakEnvVar is the legacy environment variable that opts a process
// into the soak layer without rebuilding with `-tags=soak`. It is
// preserved verbatim so existing CI workflows and developer
// workflows keep working.
const soakEnvVar = "SOAK_FULL"

// nightlyEnvVar opts a process into the nightly layer without
// rebuilding with `-tags=nightly`. Introduced alongside the
// three-layer scheme; documented in docs/test-layers.md.
const nightlyEnvVar = "GOGRAPH_NIGHTLY"

// soakEnabled reports whether the current process is running with
// the soak layer active. The nightly layer implies soak.
func soakEnabled() bool {
	if IsSoak || IsNightly {
		return true
	}
	if os.Getenv(soakEnvVar) == "1" {
		return true
	}
	if os.Getenv(nightlyEnvVar) == "1" {
		return true
	}
	return false
}

// nightlyEnabled reports whether the current process is running
// with the nightly layer active.
func nightlyEnabled() bool {
	if IsNightly {
		return true
	}
	if os.Getenv(nightlyEnvVar) == "1" {
		return true
	}
	return false
}

// RequireSoak skips the calling test unless the soak layer is
// active. A test is considered to be running under the soak layer
// when the binary was built with `-tags=soak` (or any superset such
// as `nightly`), or when the environment variable SOAK_FULL=1 is
// set, or when GOGRAPH_NIGHTLY=1 is set (nightly implies soak).
//
// Prefer placing soak-only tests in their own file with a
// `//go:build soak` header. Use RequireSoak only when splitting the
// test out is impractical.
func RequireSoak(tb testing.TB) {
	tb.Helper()
	if soakEnabled() {
		return
	}
	tb.Skipf("test requires soak layer (build with -tags=soak or set %s=1)", soakEnvVar)
}

// RequireNightly skips the calling test unless the nightly layer is
// active. A test is considered to be running under the nightly
// layer when the binary was built with `-tags=nightly`, or when the
// environment variable GOGRAPH_NIGHTLY=1 is set.
//
// Prefer placing nightly-only tests in their own file with a
// `//go:build nightly` header. Use RequireNightly only when
// splitting the test out is impractical.
func RequireNightly(tb testing.TB) {
	tb.Helper()
	if nightlyEnabled() {
		return
	}
	tb.Skipf("test requires nightly layer (build with -tags=nightly or set %s=1)", nightlyEnvVar)
}
