// Package version pins the single source of truth for the idemgate version.
//
// Everything that prints a version (the CLI banner, the serve startup line,
// the smoke test) reads this constant, so a release bump is a one-line
// change here plus a CHANGELOG entry.
package version

// Version is the semantic version of idemgate.
// Keep CHANGELOG.md in lockstep when changing it.
const Version = "0.1.0"
