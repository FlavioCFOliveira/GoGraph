package tck

import "io/fs"

// FeatureFiles returns the embedded openCypher TCK feature file tree.
// It is exposed for use by the external test package (tck_test) so that the
// godog runner can load feature files from the embedded FS without importing
// godog in non-test production code.
func FeatureFiles() fs.FS { return featureFiles }
