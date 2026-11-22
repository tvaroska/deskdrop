#!/usr/bin/env bash
# Canonical verification gate for deskdrop.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

GOCACHE="${GOCACHE:-/tmp/go-build-deskdrop}"
mkdir -p "${GOCACHE}"
export GOCACHE

unformatted="$(gofmt -s -l .)"
if [[ -n "${unformatted}" ]]; then
  echo "ERROR: run 'gofmt -s -w' on:" >&2
  echo "${unformatted}" >&2
  exit 1
fi

go vet ./...
GOOS=windows GOARCH=amd64 go vet ./cmd/deskdrop ./internal/driver/...
if [[ $# -eq 0 ]]; then
  go test -race ./...
else
  go test "$@"
fi
