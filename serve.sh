#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"
# exec podman run --rm -it --userns=keep-id --network=host -v "$PWD":/workspace -w /workspace yocache-dev ./yocache --addr 127.0.0.1:6768
exec ./yocache --addr 127.0.0.1:6768 --evict lru