#!/usr/bin/env bash

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

VERSION="${1:?Usage: build-artifacts.sh <version> <output-dir> [build-id]}"
VERSION="${VERSION#v}"
VERSION="${VERSION#V}"
OUT_DIR="${2:?An output directory is required}"
BUILD_ID="${3:-$(date -u +%Y%m%d).1}"
VERSION_PATTERN='^[0-9]+\.[0-9]+\.[0-9]+(-?[0-9A-Za-z][0-9A-Za-z-]*(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'

[[ "${VERSION}" =~ ${VERSION_PATTERN} ]] || { printf 'Invalid release version: %s\n' "${VERSION}" >&2; exit 1; }
[[ "${BUILD_ID}" =~ ^[0-9A-Za-z._-]+$ ]] || { printf 'Invalid build ID: %s\n' "${BUILD_ID}" >&2; exit 1; }

COMMIT="$(git rev-parse --short HEAD)"
BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
VER_PKG="github.com/abdorrahmani/phelix/internal/version"
SOURCE_PATHS="$(pwd);$(go env GOMODCACHE);$(go env GOROOT)/src"
LDFLAGS="-s -w -X ${VER_PKG}.Version=${VERSION} -X ${VER_PKG}.BuildID=${BUILD_ID} -X ${VER_PKG}.Commit=${COMMIT} -X ${VER_PKG}.BuildTime=${BUILD_TIME}"
CONFIG_BACKUP="$(mktemp)"
cp config/config.yml "${CONFIG_BACKUP}"

# restore_config returns the checkout to its original configuration after a build.
restore_config() {
    cp "${CONFIG_BACKUP}" config/config.yml
    rm -f "${CONFIG_BACKUP}"
}
trap restore_config EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

sed -E -i \
    -e 's@^([[:space:]]*mode:).*@\1 "production"@' \
    -e 's@^([[:space:]]*api:).*@\1 "https://phelix.anophel.com/api/v1"@' \
    -e 's@^([[:space:]]*grpcUrl:).*@\1 "phelix.anophel.com:443"@' \
    config/config.yml

mkdir -p "${OUT_DIR}"

# build creates a binary for a release platform with its version information.
build() {
    local os="$1" arch="$2" name="$3" goarm="${4:-}"
    local -a envs=(CGO_ENABLED=0 GOOS="${os}" GOARCH="${arch}")
    if [[ -n "${goarm}" ]]; then
        envs+=(GOARM="${goarm}")
    fi
    printf 'Building %s\n' "${name}"
    env "${envs[@]}" go build -trimpath=false \
        -gcflags="all=-trimpath \"${SOURCE_PATHS}\"" \
        -asmflags="all=-trimpath \"${SOURCE_PATHS}\"" \
        -ldflags="${LDFLAGS}" -o "${OUT_DIR}/${name}" .
    (
        cd "${OUT_DIR}"
        sha256sum "${name}" > "${name}.sha256"
    )
}

build linux amd64 phelix-linux-amd64
build linux arm64 phelix-linux-arm64
build linux arm phelix-linux-armv7 7
build linux arm phelix-linux-armv6 6
build darwin amd64 phelix-darwin-amd64
build darwin arm64 phelix-darwin-arm64

(
    cd "${OUT_DIR}"
    cat phelix-linux-{amd64,arm64,armv7,armv6}.sha256 phelix-darwin-{amd64,arm64}.sha256 > checksums.sha256
    sha256sum -c checksums.sha256
    printf '%s\n' "${VERSION}" > version
)
printf 'Built Phelix %s with build ID %s from %s\n' "${VERSION}" "${BUILD_ID}" "${COMMIT}"
