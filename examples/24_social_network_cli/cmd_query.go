package main

// cmdQuery runs a Cypher query against the data directory and emits each
// record as a JSON Lines record. The real implementation lands in
// sprint 55 task T5 (#492); this stub validates the shared -d flag and
// returns errNotImplemented so the dispatcher can be tested in isolation.
func cmdQuery(args []string) error {
	if _, _, err := parseDataDir("query", args); err != nil {
		return err
	}
	return errNotImplemented
}
