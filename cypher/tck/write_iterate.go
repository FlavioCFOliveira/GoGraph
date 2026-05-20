package tck

import "strings"

// WriteFeatureDirs lists the TCK feature directories that contain write-clause scenarios.
var WriteFeatureDirs = []string{
	"features/clauses/create",
	"features/clauses/merge",
	"features/clauses/delete",
	"features/clauses/set",
	"features/clauses/remove",
}

// LoadWriteScenarios returns all scenarios from write-clause feature files.
// It is a filtered view of [LoadScenarios], restricted to the directories
// listed in [WriteFeatureDirs]. Each returned [Scenario] has its SkipReason
// field set by [classifySkip], including scenarios expanded from Scenario
// Outline blocks.
//
// LoadWriteScenarios is safe to call concurrently.
func LoadWriteScenarios() ([]*Scenario, error) {
	all, err := LoadScenarios()
	if err != nil {
		return nil, err
	}
	var out []*Scenario
	for _, s := range all {
		for _, dir := range WriteFeatureDirs {
			if strings.HasPrefix(s.File, dir) {
				out = append(out, s)
				break
			}
		}
	}
	return out, nil
}
