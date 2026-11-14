//go:build sqlite_fts5

package main

// ftsBuildTagPresent is referenced by fts_tag_missing.go so that a build
// without -tags sqlite_fts5 fails to compile.
const ftsBuildTagPresent = true
