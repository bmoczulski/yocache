#!/bin/sh
set -e

TARGET_UID=10001
TARGET_GID=10001
DATA_DIR=/var/lib/yocache

if [ "$(id -u)" = "0" ]; then
    current="$(stat -c '%u:%g' "$DATA_DIR" 2>/dev/null || echo '')"
    if [ "$current" != "${TARGET_UID}:${TARGET_GID}" ]; then
        chown -R "${TARGET_UID}:${TARGET_GID}" "$DATA_DIR"
    fi
    exec su-exec "${TARGET_UID}:${TARGET_GID}" /usr/local/bin/yocache "$@"
else
    exec /usr/local/bin/yocache "$@"
fi
