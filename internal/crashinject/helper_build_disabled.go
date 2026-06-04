//go:build !gograph_crashinject

package crashinject

// helperBuildTags is empty in the default (no-tag) build: the
// crashinject-helper is compiled with the production no-op
// crashpoint.Breakpoint, so a spawned helper never self-kills regardless
// of GOGRAPH_CRASH_AT. This is what the untagged guard test relies on to
// prove the breakpoint is inert without the gograph_crashinject tag.
var helperBuildTags []string

// helperBinSuffix is empty for the no-op build; see the enabled variant
// for the rationale behind keeping the two cached binaries distinct.
const helperBinSuffix = ""
