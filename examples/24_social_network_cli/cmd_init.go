package main

// cmdInit opens or creates the data directory. The real implementation
// lands in sprint 55 task T4 (#491); this stub validates the shared
// -d flag and returns errNotImplemented so the dispatcher and exit-code
// mapping can be exercised independently.
func cmdInit(args []string) error {
	if _, _, err := parseDataDir("init", args); err != nil {
		return err
	}
	return errNotImplemented
}
