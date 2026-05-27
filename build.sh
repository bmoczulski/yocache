#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"
podman run --rm -it --userns=keep-id \
  -v "$PWD":/workspace -w /workspace yocache-dev \
  sh -c 'go build ./cmd/... && go vet ./cmd/... && go test -race ./cmd/...'