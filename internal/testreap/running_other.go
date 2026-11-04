//go:build integration && !unix

package testreap

// running cannot tell here, so every process counts as running and Reap
// removes nothing.
func running(int) bool { return true }
