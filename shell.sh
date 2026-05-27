#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"
exec podman run --rm -it --userns=keep-id   -v "$PWD":/workspace -w /workspace yocache-dev