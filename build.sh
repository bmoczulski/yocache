#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"
podman run --rm -it --userns=keep-id \
  -v "$PWD":/workspace -w /workspace yocache-dev \
  sh -c 'CGO_ENABLED=0 go build -trimpath ./cmd/... && go vet ./cmd/... && go test -race ./cmd/...'