#!/usr/bin/env bash

set -euo pipefail

TAG="${1:?Usage: publish-github.sh <tag> <artifact-dir>}"
OUT_DIR="${2:?An artifact directory is required}"
source "$(dirname "${BASH_SOURCE[0]}")/release-tag.sh"
read_release_tag "${TAG}"
command -v gh >/dev/null || { printf 'Install the GitHub CLI before publishing.\n' >&2; exit 1; }
: "${GH_REPO:?GH_REPO must identify the release repository}"

[[ "$(cat "${OUT_DIR}/version")" == "${VERSION}" ]] || { printf 'The artifacts do not match the release tag.\n' >&2; exit 1; }
ARTIFACTS=(phelix-linux-amd64 phelix-linux-arm64 phelix-linux-armv7 phelix-linux-armv6 phelix-darwin-amd64 phelix-darwin-arm64)
FILES=()
for name in "${ARTIFACTS[@]}"; do
    [[ -s "${OUT_DIR}/${name}" && -s "${OUT_DIR}/${name}.sha256" ]] || { printf 'Missing release artifact: %s\n' "${name}" >&2; exit 1; }
    FILES+=("${OUT_DIR}/${name}" "${OUT_DIR}/${name}.sha256")
done
for example in docker-host-controller docker-container-controller; do
    if [[ -f "$OUT_DIR/$example.tar.gz" ]]; then
        (cd "$OUT_DIR" && sha256sum -c "$example.tar.gz.sha256")
        FILES+=("$OUT_DIR/$example.tar.gz" "$OUT_DIR/$example.tar.gz.sha256")
    fi
done
FILES+=("${OUT_DIR}/checksums.sha256" "${OUT_DIR}/version")
(
    cd "${OUT_DIR}"
    sha256sum -c checksums.sha256
    for name in "${ARTIFACTS[@]}"; do
        sha256sum -c "${name}.sha256"
    done
)

if existing="$(gh release view "${TAG}" --repo "${GH_REPO}" --json isDraft --jq '.isDraft' 2>/dev/null)"; then
    [[ "${existing}" == true ]] || { printf 'Release %s is already published; its assets will not be replaced.\n' "${TAG}" >&2; exit 1; }
else
    gh release create "${TAG}" --repo "${GH_REPO}" --verify-tag --draft \
        --title "Phelix ${VERSION}" --generate-notes --prerelease="${PRERELEASE}"
fi

gh release upload "${TAG}" "${FILES[@]}" --repo "${GH_REPO}" --clobber
LATEST=true
if [[ "${PRERELEASE}" == true ]]; then
    LATEST=false
fi
gh release edit "${TAG}" --repo "${GH_REPO}" --draft=false \
    --prerelease="${PRERELEASE}" --latest="${LATEST}"
printf 'Published Phelix %s on GitHub\n' "${VERSION}"
