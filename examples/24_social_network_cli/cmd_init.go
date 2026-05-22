package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// cmdInit opens or creates the data directory and writes an initial
// empty snapshot if no manifest is present, so subsequent subcommands
// can reopen it via recovery.Open without further bootstrap. The
// operation is idempotent: running `init` against a directory that
// already holds a manifest is a no-op.
//
// On success cmdInit writes a single JSON object to stdout:
//
//	{"data_dir":"<abs path>","status":"ok"}
//
// The absolute path is resolved via filepath.Abs so the reply is
// stable regardless of the caller's working directory.
func cmdInit(args []string) error {
	dir, _, err := parseDataDir("init", args)
	if err != nil {
		return err
	}
	if err := initEmpty(dir); err != nil {
		return fmt.Errorf("init: %w", err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("init: resolve %q: %w", dir, err)
	}
	return writeJSONObject(os.Stdout, map[string]any{
		"status":   "ok",
		"data_dir": abs,
	})
}
