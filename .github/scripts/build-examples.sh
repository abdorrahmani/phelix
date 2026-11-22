#!/usr/bin/env bash
set -euo pipefail
version="${1:?Usage: build-examples.sh <version> <output-dir>}"
output="${2:?An output directory is required}"
source "$(dirname "${BASH_SOURCE[0]}")/release-tag.sh"
read_release_tag "v${version#v}"
mkdir -p "$output"
output="$(cd "$output" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
for name in docker-host-controller docker-container-controller; do
    mkdir "$work/$name"
    tar -C "examples/$name" -cf - README.md setup.sh compose.yaml controller.example.yaml .env.example \
        app/Dockerfile app/.dockerignore app/go.mod app/main.go app/phelix.yaml | tar -C "$work/$name" -xf -
    printf '%s\n' "$VERSION" > "$work/$name/VERSION"
    tar -C "$work" -czf "$output/$name.tar.gz" "$name"
    (cd "$output" && sha256sum "$name.tar.gz" > "$name.tar.gz.sha256")
done
