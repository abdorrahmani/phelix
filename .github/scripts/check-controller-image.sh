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
container="$(docker run -d --network "$network" \
    --mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock \
    --mount type=bind,src="$work/state",dst=/var/lib/phelix \
    --mount type=bind,src="$work/run",dst=/run/phelix \
    --mount type=bind,src="$work/projects",dst=/projects,readonly \
    --mount type=bind,src="$work/controller.yaml",dst=/etc/phelix/controller.yaml,readonly \
    "$image")"
for ((attempt=0; attempt<60; attempt++)); do
    if docker exec "$container" phelix controller health; then break; fi
    if ((attempt == 59)); then docker logs "$container"; exit 1; fi
    sleep 1
done
docker exec "$container" phelix controller info | grep -F '"mode":"container"'
docker exec "$container" phelix list
docker stop --time=60 "$container" >/dev/null
[[ "$(docker inspect "$container" --format '{{.State.ExitCode}}')" == 0 ]]
printf 'Validated Phelix %s on linux/%s\n' "$version" "$arch"
