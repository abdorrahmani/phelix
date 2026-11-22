#!/usr/bin/env bash
set -euo pipefail
umask 077
image="${1:?Usage: check-controller-image.sh <image> <version> <arch>}"
version="${2:?Version is required}"
arch="${3:?Architecture is required}"
work="$(mktemp -d)"
network="phelix-image-check-$(basename "$work" | tr '[:upper:]' '[:lower:]')"
container=""

# cleanup removes only the resources created by this image check.
cleanup() {
    if [[ -n "$container" ]]; then docker rm -f "$container" >/dev/null; fi
    docker network rm "$network" >/dev/null
    docker run --rm --entrypoint chown --mount type=bind,src="$work",dst=/cleanup "$image" -R "$(id -u):$(id -g)" /cleanup
    rm -rf "$work"
}

[[ "$(docker image inspect "$image" --format '{{.Os}}/{{.Architecture}}')" == "linux/$arch" ]]
[[ "$(docker run --rm "$image" version --short)" == "$version" ]]
docker run --rm "$image" version | grep -F "linux/$arch"
mkdir -p "$work/state" "$work/run" "$work/projects"
docker network create "$network" >/dev/null
trap cleanup EXIT
cat > "$work/controller.yaml" <<CONFIG
mode: container
network: $network
socket: /run/phelix/control.sock
sources:
  - host: $work/projects
    controller: /projects
CONFIG
# start_controller creates a Controller using the same persistent mounts.
start_controller() {
    docker run -d --network "$network" \
    --mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock \
    --mount type=bind,src="$work/state",dst=/var/lib/phelix \
    --mount type=bind,src="$work/run",dst=/run/phelix \
    --mount type=bind,src="$work/projects",dst=/projects,readonly \
    --mount type=bind,src="$work/controller.yaml",dst=/etc/phelix/controller.yaml,readonly \
    "$image"
}

# wait_ready checks the local API after startup or restart.
wait_ready() {
    for ((attempt=0; attempt<60; attempt++)); do
        if docker exec "$container" phelix controller health; then break; fi
        if ((attempt == 59)); then docker logs "$container"; exit 1; fi
        sleep 1
    done
}
container="$(start_controller)"
wait_ready
docker exec "$container" phelix controller info | grep -F '"mode":"container"'
docker exec "$container" phelix list
mkdir -p "$work/projects/native"
printf 'module native-check\n\ngo 1.21\n' > "$work/projects/native/go.mod"
printf 'package main\nfunc main() {}\n' > "$work/projects/native/main.go"
printf 'name: native-check\nport: 18081\n' > "$work/projects/native/phelix.yaml"
if docker exec -w /projects/native "$container" phelix build native-check > "$work/native-check.log" 2>&1; then
    printf 'Container unexpectedly accepted a native build.\n' >&2
    exit 1
fi
grep -F 'native applications require Host Mode' "$work/native-check.log"
if grep -F 'Checking toolchain' "$work/native-check.log"; then exit 1; fi
identity="$(docker exec "$container" cat /var/lib/phelix/agent-id)"
docker restart --time=60 "$container" >/dev/null
wait_ready
docker exec "$container" phelix controller info | grep -F "$identity"
docker exec "$container" sh -c 'printf "%s" "{\"id\":\"interrupted-check\",\"state\":\"RUNNING\"}" > /var/lib/phelix/controller-operations/interrupted-check.json'
docker rm -f "$container" >/dev/null
container="$(start_controller)"
wait_ready
docker exec "$container" phelix controller info | grep -F "$identity"
docker exec "$container" phelix controller operation interrupted-check | grep -F 'INTERRUPTED'
if docker exec "$container" phelix update; then
    printf 'Container image unexpectedly allowed binary replacement.\n' >&2
    exit 1
fi
docker stop --time=60 "$container" >/dev/null
[[ "$(docker inspect "$container" --format '{{.State.ExitCode}}')" == 0 ]]
printf 'Validated Phelix %s on linux/%s\n' "$version" "$arch"
