#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"

# Web dashboard first — cmd/yocache/web.go embeds cmd/yocache/web/dist via
# //go:embed, so `go build` fails if dist/ is absent. This is intentional:
# we never ship a dashboard-less binary from a normal build path.
./build-web.sh

podman run --rm -it --userns=keep-id \
  -v "$PWD":/workspace -w /workspace yocache-dev \
  sh -c 'CGO_ENABLED=0 go build -trimpath ./cmd/... && go vet ./cmd/... && go test -race ./cmd/...'