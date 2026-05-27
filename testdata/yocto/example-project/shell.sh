#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"
KAS_IMAGE_VERSION=5.2 KAS_CONTAINER_ENGINE=podman \
  exec ../bin/kas-container-5.2 --runtime-args "--network=host" \
  shell yocache.yml "$@"