#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"

# Run the Vite dev server (HMR + proxy to the yocache Go backend on :6768)
# in the same node:24-alpine container as build-web.sh, so the musl-linked
# node_modules stays valid across both. --network=host is what makes this
# ergonomic:
#   * vite's config proxies /api → http://localhost:6768 from *inside* the
#     container; with host networking, that "localhost" is the actual host,
#     so it reaches the yocache Go server directly.
#   * vite binds to :5173 which is now the host's :5173 — open
#     http://localhost:5173 in a browser as usual.
#
# Start the yocache Go server separately in another terminal (./serve.sh).
# Save any .svelte / .ts / .css and the browser hot-reloads in <100ms.
#
# If HMR stops picking up file changes (rare on rootless podman, more common
# on macOS/WSL bind mounts), set CHOKIDAR_USEPOLLING=true in the env below.

if [ ! -d cmd/yocache/web/node_modules ]; then
  echo "cmd/yocache/web/node_modules is missing — run ./build-web.sh first"
  exit 1
fi

podman run --rm -it --userns=keep-id --network=host \
  -v "$PWD/cmd/yocache/web":/w -w /w \
  node:24-alpine \
  npm run dev
