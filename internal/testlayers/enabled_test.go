package testlayers

import (
	"os"
	"testing"
)

// These tests exercise the env-var driven branches of soakEnabled and
// nightlyEnabled, and both the skip and admit paths of RequireSoak and
// RequireNightly. They run in the default (short) build where the compile
// time constants IsSoak and IsNightly are both false, so the runtime
// env-var opt-ins are the only way to flip the helpers — exactly the
// branches the build-tag-gated sample tests cannot reach.
//
// t.Setenv forbids t.Parallel, so these tests run serially; each restores
// the environment via t.Setenv's own cleanup.

// clearLayerEnv unsets both layer env vars for the duration of the test so
// a developer's ambient SOAK_FULL / GOGRAPH_NIGHTLY does not leak into the
// "disabled" assertions. t.Setenv registers cleanup that restores the
// previous value automatically.
func clearLayerEnv(t *testing.T) {
	t.Helper()
	t.Setenv(soakEnvVar, "")
	t.Setenv(nightlyEnvVar, "")
	if err := os.Unsetenv(soakEnvVar); err != nil {
		t.Fatalf("unset %s: %v", soakEnvVar, err)
	}
	if err := os.Unsetenv(nightlyEnvVar); err != nil {
		t.Fatalf("unset %s: %v", nightlyEnvVar, err)
	}
}

func TestSoakEnabled_EnvMatrix(t *testing.T) {
	if IsSoak || IsNightly {
		t.Skip("compile-time soak/nightly tags active; env-matrix branches are pre-empted")
	}

	t.Run("all_unset", func(t *testing.T) {
		clearLayerEnv(t)
		if soakEnabled() {
			t.Fatal("soakEnabled() = true with no env opt-in")
		}
	})

	t.Run("soak_env_set", func(t *testing.T) {
		clearLayerEnv(t)
		t.Setenv(soakEnvVar, "1")
		if !soakEnabled() {
			t.Fatalf("soakEnabled() = false with %s=1", soakEnvVar)
		}
	})

	t.Run("soak_env_non_one", func(t *testing.T) {
		clearLayerEnv(t)
		// Any value other than the literal "1" must not enable the layer.
		t.Setenv(soakEnvVar, "true")
		if soakEnabled() {
			t.Fatalf("soakEnabled() = true with %s=true (only \"1\" enables)", soakEnvVar)
		}
	})

	t.Run("nightly_env_implies_soak", func(t *testing.T) {
		clearLayerEnv(t)
		t.Setenv(nightlyEnvVar, "1")
		if !soakEnabled() {
			t.Fatalf("soakEnabled() = false with %s=1 (nightly implies soak)", nightlyEnvVar)
		}
	})
}

func TestNightlyEnabled_EnvMatrix(t *testing.T) {
	if IsNightly {
		t.Skip("compile-time nightly tag active; env-matrix branches are pre-empted")
	}

	t.Run("all_unset", func(t *testing.T) {
		clearLayerEnv(t)
		if nightlyEnabled() {
			t.Fatal("nightlyEnabled() = true with no env opt-in")
		}
	})

	t.Run("nightly_env_set", func(t *testing.T) {
		clearLayerEnv(t)
		t.Setenv(nightlyEnvVar, "1")
		if !nightlyEnabled() {
			t.Fatalf("nightlyEnabled() = false with %s=1", nightlyEnvVar)
		}
	})

	t.Run("nightly_env_non_one", func(t *testing.T) {
		clearLayerEnv(t)
		t.Setenv(nightlyEnvVar, "yes")
		if nightlyEnabled() {
			t.Fatalf("nightlyEnabled() = true with %s=yes (only \"1\" enables)", nightlyEnvVar)
		}
	})

	t.Run("soak_env_does_not_imply_nightly", func(t *testing.T) {
		clearLayerEnv(t)
		// SOAK_FULL enables soak but must NOT enable nightly.
		t.Setenv(soakEnvVar, "1")
		if nightlyEnabled() {
			t.Fatalf("nightlyEnabled() = true with only %s=1 (soak must not imply nightly)", soakEnvVar)
		}
	})
}

func TestRequireSoak_SkipAndAdmit(t *testing.T) {
	if IsSoak || IsNightly {
		t.Skip("compile-time soak/nightly tags active; the runtime skip path is unreachable")
	}

	t.Run("skips_when_disabled", func(t *testing.T) {
		clearLayerEnv(t)
		// RequireSoak must skip the inner sub-test: it calls Skipf, which
		// runs runtime.Goexit, so the line after it is never reached and
		// reachedBody stays false.
		reachedBody := false
		t.Run("inner", func(inner *testing.T) {
			RequireSoak(inner)
			reachedBody = true
		})
		if reachedBody {
			t.Fatal("RequireSoak admitted the test with no soak opt-in")
		}
	})

	t.Run("admits_when_enabled", func(t *testing.T) {
		clearLayerEnv(t)
		t.Setenv(soakEnvVar, "1")
		// RequireSoak must NOT skip; the body after the call must run.
		reachedBody := false
		t.Run("inner", func(inner *testing.T) {
			RequireSoak(inner)
			reachedBody = true
		})
		if !reachedBody {
			t.Fatal("RequireSoak skipped despite SOAK_FULL=1")
		}
	})
}

func TestRequireNightly_SkipAndAdmit(t *testing.T) {
	if IsNightly {
		t.Skip("compile-time nightly tag active; the runtime skip path is unreachable")
	}

	t.Run("skips_when_disabled", func(t *testing.T) {
		clearLayerEnv(t)
		reachedBody := false
		t.Run("inner", func(inner *testing.T) {
			RequireNightly(inner)
			reachedBody = true
		})
		if reachedBody {
			t.Fatal("RequireNightly admitted the test with no nightly opt-in")
		}
	})

	t.Run("admits_when_enabled", func(t *testing.T) {
		clearLayerEnv(t)
		t.Setenv(nightlyEnvVar, "1")
		reachedBody := false
		t.Run("inner", func(inner *testing.T) {
			RequireNightly(inner)
			reachedBody = true
		})
		if !reachedBody {
			t.Fatal("RequireNightly skipped despite GOGRAPH_NIGHTLY=1")
		}
	})
}
