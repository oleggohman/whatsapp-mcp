#!/usr/bin/env bash
# Runs the bridge tests with statement coverage and fails unless the total is 100.0%.
#
# Documented exclusions (the only code allowed to stay uncovered):
#   - main_entry.go: holds only `func main() { os.Exit(run()) }`. It ends the process, so a
#     test cannot call it; everything it runs (run) is covered. Dropped from the profile below.
#   - lock_windows.go: built only for Windows (build-tagged), so it is not compiled on the
#     darwin/linux hosts that run this script and never appears in the profile.
#
# Usage: bash scripts/coverage.sh   (from anywhere; extra args go to `go test`)
set -euo pipefail

cd "$(dirname "$0")/.."

tmp="$(mktemp -d "${TMPDIR:-/tmp}/bridge-cover.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

go test -count=1 -coverprofile="$tmp/raw.out" "$@" ./...

# Keep the mode line and every block that is not in an excluded file.
grep -v '/main_entry\.go:' "$tmp/raw.out" > "$tmp/cover.out"

go tool cover -func="$tmp/cover.out" | awk '$NF != "100.0%"'

total="$(go tool cover -func="$tmp/cover.out" | awk '/^total:/ { print $NF }')"
echo "Total statement coverage (excluding main_entry.go): $total"

if [ "${total%\%}" != "100.0" ]; then
  echo "FAIL: coverage is below 100.0%" >&2
  exit 1
fi
