#!/usr/bin/env bash

# read_release_tag checks the tag and identifies stable and beta releases.
read_release_tag() {
    local tag="$1"
    local pattern='^v[0-9]+\.[0-9]+\.[0-9]+(-beta(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
    if [[ ! "${tag}" =~ ${pattern} ]]; then
        printf 'Invalid release tag: %s. Use v1.3.0 or v1.3.0-beta.1.\n' "${tag}" >&2
        return 1
    fi
    VERSION="${tag#v}"
    IMAGE_TAG="${VERSION//+/_}"
    PRERELEASE=false
    if [[ "${VERSION%%+*}" == *-beta* ]]; then
        PRERELEASE=true
    fi
}
