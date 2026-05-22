package main

// cmdSnapshot forces a manual snapshot of the current in-memory graph.
// The real implementation lands in sprint 55 task T7 (#494); this stub
// validates the shared -d flag and returns errNotImplemented so the
// dispatcher can be tested in isolation.
func cmdSnapshot(args []string) error {
	if _, _, err := parseDataDir("snapshot", args); err != nil {
		return err
	}
	return errNotImplemented
}
