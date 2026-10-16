// Package buildinfo carries build metadata stamped into the binary at
// compile time via -ldflags (see the Makefile's build target).
package buildinfo

// Commit is the short git hash the binary was built from. It defaults to
// "dev" when the stamp is not applied (e.g. plain `go run`), which the UI
// treats as "no build info" and hides.
var Commit = "dev"

// RepoURL is the public repository; per-commit links are derived from it.
const RepoURL = "https://github.com/kzrl/disjoint"
