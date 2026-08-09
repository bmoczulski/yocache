#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"

# Build the Svelte SPA that cmd/yocache/web.go embeds. Runs in a stock
# node:24-alpine — no Node install on the host needed, and the Go devcontainer
# stays language-pure. In CI, workflows install Node natively via
# actions/setup-node instead of using this script (see .github/workflows/*).

stamp=cmd/yocache/web/dist/index.html

# Skip the container spin-up when no source or config file is newer than the
# previous build's index.html. `find -print -quit` short-circuits at the first
# match. Force a rebuild with `rm cmd/yocache/web/dist/index.html`.
if [ -f "$stamp" ]; then
  newer=$(find \
    cmd/yocache/web/src \
    cmd/yocache/web/package.json \
    cmd/yocache/web/package-lock.json \
    cmd/yocache/web/vite.config.ts \
    cmd/yocache/web/svelte.config.js \
    cmd/yocache/web/tsconfig.json \
    cmd/yocache/web/tsconfig.node.json \
    cmd/yocache/web/index.html \
    -newer "$stamp" -print -quit 2>/dev/null)
  if [ -z "$newer" ]; then
    echo "web/dist is up-to-date with sources — skipping build"
    exit 0
  fi
fi

podman run --rm -it --userns=keep-id \
  -v "$PWD/cmd/yocache/web":/w -w /w \
  node:24-alpine \
  sh -c 'npm ci && npm run build'
