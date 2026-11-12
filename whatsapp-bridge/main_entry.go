package main

import "os"

// main is kept alone in this file: it only exits with run's code, so scripts/coverage.sh
// leaves the file out of the coverage total (see the exclusions listed there).
func main() {
	os.Exit(run())
}
