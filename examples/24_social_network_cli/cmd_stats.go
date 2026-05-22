package main

// cmdStats prints node and edge counts as a single JSON object. The
// real implementation lands in sprint 55 task T8 (#495); this stub
// validates the shared -d flag and returns errNotImplemented so the
// dispatcher can be tested in isolation.
func cmdStats(args []string) error {
	if _, _, err := parseDataDir("stats", args); err != nil {
		return err
	}
	return errNotImplemented
}
