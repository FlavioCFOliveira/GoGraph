package packstream

// MaxValueDepthForTest exposes the unexported maxValueDepth bound to black-box
// tests in package packstream_test. It exists solely so the nesting-depth
// regression tests can pin their payloads to the exact boundary without
// hard-coding the constant; production code never references it.
func MaxValueDepthForTest() int { return maxValueDepth }
