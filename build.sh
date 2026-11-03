#!/usr/bin/env bash
# Builds ./wt-tui after running the tests.
set -euo pipefail
cd "$(dirname "$0")"
go test ./...
go build -o wt-tui .
echo "built $(pwd)/wt-tui"
