package main

// cmdSeed populates the data directory with the deterministic
// social-network fixture. The real implementation lands in sprint 55
// task T6 (#493); this stub validates the shared -d flag and returns
// errNotImplemented so the dispatcher can be tested in isolation.
func cmdSeed(args []string) error {
	if _, _, err := parseDataDir("seed", args); err != nil {
		return err
	}
	return errNotImplemented
}
