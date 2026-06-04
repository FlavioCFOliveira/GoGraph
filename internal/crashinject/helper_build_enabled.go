//go:build gograph_crashinject

package crashinject

// helperBuildTags are the extra `go build` arguments [buildHelperOnce]
// splices in when the crash-injection battery is compiled with the
// gograph_crashinject tag. The child crashinject-helper must carry the
// same tag so its embedded crashpoint.Breakpoint is the active SIGKILL
// implementation rather than the production no-op; otherwise the helper
// would run to completion and no crash would be injected.
var helperBuildTags = []string{"-tags", "gograph_crashinject"}

// helperBinSuffix distinguishes the cached helper binary built with the
// crash-injection tag from the no-op build, so the two never alias the
// same file on disk.
const helperBinSuffix = "-crashinject"
