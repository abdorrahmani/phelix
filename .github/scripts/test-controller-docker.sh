#!/usr/bin/env bash
set -euo pipefail
umask 077
image="${1:?Usage: test-controller-docker.sh <image> <cli> [container|host]}"
binary="$(realpath "${2:?CLI executable is required}")"
mode="${3:-container}"
[[ "$mode" == container || "$mode" == host ]]
repository="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
namespace="$(basename "$work" | tr -cd 'a-z0-9')"
network="phelix-validation-$namespace"
container=""
host_pid=""
traffic_pid=""
interrupted_cli_pid=""
apps=()
mkdir -p "$work/state" "$work/run" "$work/home" "$work/projects"

# cli reaches the Controller through its shared socket from the host.
cli() {
    PHELIX_DATA_DIR="$work/client" PHELIX_CONTROLLER_CONFIG="" PHELIX_CONTROL_SOCKET="$work/run/control.sock" "$binary" "$@"
}

# cleanup removes the isolated applications and Controller created by this test.
cleanup() {
    if [[ -n "$interrupted_cli_pid" ]]; then kill "$interrupted_cli_pid" 2>/dev/null || true; fi
    if [[ -n "$traffic_pid" ]]; then kill "$traffic_pid" 2>/dev/null || true; fi
    for app in "${apps[@]}"; do
        while read -r id; do
            if [[ -n "$id" ]]; then docker rm -f "$id" >/dev/null; fi
        done < <(docker ps -aq --filter "label=phelix.app=$app")
        while read -r id; do
            if [[ -n "$id" ]]; then docker image rm "$id" >/dev/null; fi
        done < <(docker image ls -q --filter "reference=$app:*")
    done
    if [[ -n "$host_pid" ]]; then kill -TERM "$host_pid" 2>/dev/null || true; wait "$host_pid" || true; fi
    if [[ -n "$container" ]]; then docker rm -f "$container" >/dev/null; fi
    docker network rm "$network" >/dev/null
    docker run --rm --entrypoint chown --mount type=bind,src="$work",dst=/cleanup "$image" -R "$(id -u):$(id -g)" /cleanup
    rm -rf "$work"
}

docker network create "$network" >/dev/null
trap cleanup EXIT
public_port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
controller_root=/projects
if [[ "$mode" == host ]]; then controller_root="$work/projects"; fi
cat > "$work/controller.yaml" <<CONFIG
mode: $mode
network: $network
socket: $work/run/control.sock
sources:
  - host: $work/projects
    controller: $controller_root
CONFIG
if [[ "$mode" == container ]]; then
    sed -i "s|socket: .*|socket: /run/phelix/control.sock|" "$work/controller.yaml"
    printf 'socket_uid: %s\n' "$(id -u)" >> "$work/controller.yaml"
fi

# start_controller runs the selected mode with isolated identity and state.
start_controller() {
    if [[ "$mode" == container ]]; then
        container="$(docker run -d --network "$network" -p "127.0.0.1::$public_port" \
            --mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock \
            --mount type=bind,src="$work/state",dst=/var/lib/phelix \
            --mount type=bind,src="$work/run",dst=/run/phelix \
            --mount type=bind,src="$work/projects",dst=/projects,readonly \
            --mount type=bind,src="$work/controller.yaml",dst=/etc/phelix/controller.yaml,readonly "$image")"
        address="http://$(docker port "$container" "$public_port/tcp")"
    else
        env HOME="$work/home" PHELIX_DATA_DIR="$work/state" PHELIX_CONTROLLER_CONFIG="$work/controller.yaml" PHELIX_CONTROL_SOCKET="$work/run/control.sock" "$binary" controller > "$work/controller.log" 2>&1 &
        host_pid=$!
        address="http://127.0.0.1:$public_port"
    fi
    for ((attempt=0; attempt<60; attempt++)); do
        if cli controller health; then return; fi
        sleep 1
    done
    if [[ "$mode" == container ]]; then docker logs "$container"; else cat "$work/controller.log"; fi
    return 1
}

# wait_application waits for recovered application traffic before checking its version.
wait_application() {
    for ((attempt=0; attempt<20; attempt++)); do
        if curl -fsS "$address/health"; then return; fi
        sleep 1
    done
    if [[ "$mode" == container ]]; then docker logs "$container"; else cat "$work/controller.log"; fi
    cli status "$app"
    return 1
}

# project_config supplies a short verification window for the real rollout checks.
project_config() {
    local strategy="$1"
    cat > "$work/projects/app/phelix.yaml" <<CONFIG
name: $app
port: $public_port
deploy:
  runtime: docker
  strategy: $strategy
  network: $network
health:
  endpoints:
    - name: readiness
      path: /health
      mode: auto
CONFIG
    if [[ "$strategy" == rolling ]]; then sed -i '/  strategy: rolling/a\  replicas: 2' "$work/projects/app/phelix.yaml"; fi
    if [[ "$strategy" == canary ]]; then
        sed -i '/  network:/a\  rollout:\n    canary: 25%\n    duration: 2s\n    verification:\n      interval: 500ms' "$work/projects/app/phelix.yaml"
    elif [[ "$strategy" == progressive ]]; then
        sed -i '/  network:/a\  rollout:\n    steps:\n      - {traffic: 25%, duration: 1s}\n      - {traffic: 75%, duration: 1s}\n      - {traffic: 100%}\n    verification:\n      interval: 500ms' "$work/projects/app/phelix.yaml"
    fi
}

start_controller
identity="$(cli controller info | python3 -c 'import json,sys; print(json.load(sys.stdin)["agentId"])')"
for strategy in blue-green rolling canary progressive; do
    app="phelix-$namespace-$strategy"
    apps+=("$app")
    mkdir -p "$work/projects/app"
    cp "$repository"/examples/docker-container-controller/app/{Dockerfile,.dockerignore,go.mod,main.go} "$work/projects/app/"
    project_config blue-green
    cd "$work/projects/app"
    cli build "$app"
    [[ "$(curl -fsS "$address/")" == 'Phelix example v1' ]]
    cli env set "$app" CHECK_VALUE=preserved
    project_config "$strategy"
    sed -i 's/var version = "v1"/var version = "v2"/' main.go
    (
        while true; do
            curl -fsS "$address/" >> "$work/traffic.log" || printf 'FAILED\n' >> "$work/traffic.log"
            sleep 0.05
        done
    ) &
    traffic_pid=$!
    cd "$work"
    if [[ "$mode" == container ]]; then docker exec "$container" phelix deploy "$app"; else cli deploy "$app"; fi
    kill "$traffic_pid"
    wait "$traffic_pid" || true
    traffic_pid=""
    if grep -q FAILED "$work/traffic.log"; then cat "$work/traffic.log"; exit 1; fi
    [[ "$(curl -fsS "$address/")" == 'Phelix example v2' ]]
    cli list
    cli status "$app"
    cli controller runtime "$app" | python3 -c 'import json,sys; a=json.load(sys.stdin)["applications"][0]; assert a["status"]=="running"; live=[i for i in a["instances"] if i.get("status")=="running"]; assert live and all(int(i["pid"])>0 and i.get("containerId") and i.get("endpoint") for i in live); assert len({i["endpoint"] for i in live})==len(live); assert all(i["endpoint"].startswith("127.0.0.1:")== (sys.argv[1]=="host") for i in live)' "$mode"
    cli logs "$app" --no-follow --lines=20
    sleep 5
    cli controller metrics "$app"
    cli rollback "$app" --to=v1 --reason=validation --verify=1s
    [[ "$(curl -fsS "$address/")" == 'Phelix example v1' ]]
    cli controller history "$app"
    if [[ "$strategy" == blue-green ]]; then
        sed -i 's/fmt.Fprintln(w, "ready")/http.Error(w, "unavailable", http.StatusServiceUnavailable)/' "$work/projects/app/main.go"
        if cli deploy "$app" > "$work/candidate-failure.log" 2>&1; then
            printf 'Unhealthy candidate was unexpectedly promoted.\n' >&2
            exit 1
        fi
        [[ "$(curl -fsS "$address/")" == 'Phelix example v1' ]]
        cli controller runtime "$app" | python3 -c 'import json,sys; a=json.load(sys.stdin)["applications"][0]; assert a["status"]=="running" and int(a["version"])==1'
        cp "$repository/examples/docker-container-controller/app/main.go" "$work/projects/app/main.go"
        if [[ "$mode" == container ]]; then
            sed -i '1aRUN sleep 30' "$work/projects/app/Dockerfile"
            cli deploy "$app" > "$work/interrupted-build.log" 2>&1 &
            interrupted_cli_pid=$!
            for ((attempt=0; attempt<60; attempt++)); do
                if grep -q 'sleep 30' "$work/interrupted-build.log"; then break; fi
                if ((attempt == 59)); then cat "$work/interrupted-build.log"; exit 1; fi
                sleep 0.5
            done
            docker rm -f "$container" >/dev/null
            container=""
            if wait "$interrupted_cli_pid"; then printf 'Interrupted build unexpectedly succeeded.\n' >&2; exit 1; fi
            interrupted_cli_pid=""
            operation="$(python3 -c 'import re,sys; text=open(sys.argv[1]).read(); match=re.search(r"operation ([0-9a-f-]+) stream ended", text); assert match, text; print(match.group(1))' "$work/interrupted-build.log")"
            sed -i '/^RUN sleep 30$/d' "$work/projects/app/Dockerfile"
            start_controller
            wait_application
            cli controller operation "$operation" | python3 -c 'import json,sys; assert json.load(sys.stdin)["state"]=="INTERRUPTED"'
            [[ "$(curl -fsS "$address/")" == 'Phelix example v1' ]]
            printf 'Validated interrupted container build recovery\n'
        fi
    fi
    if [[ "$mode" == container ]]; then
        docker rm -f "$container" >/dev/null
        container=""
    else
        kill -TERM "$host_pid"
        wait "$host_pid"
        host_pid=""
    fi
    start_controller
    wait_application
    [[ "$(cli controller info | python3 -c 'import json,sys; print(json.load(sys.stdin)["agentId"])')" == "$identity" ]]
    [[ "$(curl -fsS "$address/")" == 'Phelix example v1' ]]
    cli stop "$app"
    cli start "$app"
    cli restart "$app"
    [[ "$(curl -fsS "$address/health")" == ready ]]
    cli remove "$app" --purge
    printf 'Validated %s strategy %s\n' "$mode" "$strategy"
done
