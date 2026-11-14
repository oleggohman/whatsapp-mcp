//go:build !sqlite_fts5

package main

// This file only compiles without the sqlite_fts5 build tag. It references a
// constant that exists only with the tag, so the build fails with
// "undefined: ftsBuildTagPresent". Rebuild with -tags sqlite_fts5.
var _ = ftsBuildTagPresent
