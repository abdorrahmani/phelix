#!/usr/bin/env bash
#
# install.sh, Phelix one-line installer (hosted at phelix.anophel.com/install.sh)
#
#   curl -fsSL https://phelix.anophel.com/install.sh | bash
#
# What it does:
#   1. Detects the operating system and CPU architecture.
#   2. Downloads the matching prebuilt Phelix binary from
#      https://phelix.anophel.com/releases/<version>/phelix-<os>-<arch>
#      (so the user does NOT need Go/Rust preinstalled).
#   3. Verifies an optional SHA-256 checksum when one is published alongside
#      the binary; skips gracefully when none is present.
#   4. Installs it to <install-dir>/phelix (default /usr/local/bin).
#   5. On Linux: registers + enables a phelix.service systemd unit that runs
#      `phelix controller` directly in the foreground. The service restores the
#      managed apps that were previously running. Authenticated backend
#      monitoring uses TLS gRPC and reconnects with exponential backoff.
#      On macOS: installs the binary only (no systemd).
#
# This is the end-user counterpart of the in-repo setup.sh, which builds from
# source for developers instead of downloading a prebuilt binary.
#
# Usage:
#   curl -fsSL https://phelix.anophel.com/install.sh | bash           # latest
#   curl -fsSL https://phelix.anophel.com/install.sh | bash -s -- --version 1.2.3
#   PHELIX_VERSION=1.2.3 curl -fsSL .../install.sh | bash
#   curl -fsSL .../install.sh | bash -s -- --uninstall
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
# This is the authoritative release contract. The in-binary `phelix update`
# command (internal/update) resolves and downloads releases from the same
# layout below; keep the two in sync when changing it.
BASE_URL="${PHELIX_RELEASE_BASE_URL:-https://phelix.anophel.com}"
VERSION="${PHELIX_VERSION:-latest}"
CHANNEL="${PHELIX_CHANNEL:-stable}"
INSTALL_DIR="/usr/local/bin"
SERVICE_NAME="phelix"
NO_SERVICE=0
UNINSTALL=0
DOCKER_MODE=0
WITH_CLI=0
CONTROLLER_CONFIG_DIR=/etc/phelix
CONTROLLER_DATA_DIR=/var/lib/phelix
CONTROLLER_SOCKET_DIR=/var/run/phelix
PROJECTS_DIR=/srv/phelix/projects
CONTROLLER_NETWORK=phelix
PROXY_PORTS=()

# ---------------------------------------------------------------------------
# Pretty output (disabled when stdout is not a TTY, e.g. piped)
# ---------------------------------------------------------------------------
if [ -t 1 ] && command -v tput >/dev/null 2>&1; then
    C_RESET="$(tput sgr0)"
    C_BOLD="$(tput bold)"
    C_BLUE="$(tput setaf 4)"
    C_GREEN="$(tput setaf 2)"
    C_YELLOW="$(tput setaf 3)"
    C_RED="$(tput setaf 1)"
    C_CYAN="$(tput setaf 6)"
else
    C_RESET=""; C_BOLD=""; C_BLUE=""; C_GREEN=""; C_YELLOW=""; C_RED=""; C_CYAN=""
fi

info()    { printf "%s▸%s %s\n"  "${C_BLUE}${C_BOLD}"   "${C_RESET}" "$*"; }
success() { printf "%s✓%s %s\n"  "${C_GREEN}${C_BOLD}"  "${C_RESET}" "$*"; }
warn()    { printf "%s⚠%s %s\n"  "${C_YELLOW}${C_BOLD}" "${C_RESET}" "$*"; }
error()   { printf "%s✗%s %s\n"  "${C_RED}${C_BOLD}"    "${C_RESET}" "$*" >&2; }
fail()    { error "$*"; exit 1; }

# ---------------------------------------------------------------------------
# Temp directory with cleanup on exit
# ---------------------------------------------------------------------------
TMPDIR_WORK="$(mktemp -d)"
cleanup() {
    rm -rf "${TMPDIR_WORK}"
}
trap cleanup EXIT INT TERM

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
need_cmd() {
    command -v "$1" >/dev/null 2>&1 || fail "required command not found: '$1'"
}

have_cmd() {
    command -v "$1" >/dev/null 2>&1
}

# ---------------------------------------------------------------------------
# Progress output
# ---------------------------------------------------------------------------
# Live progress bars are painted only when stdout is a TTY (so `curl | bash`
# one-liners show them, but piped/scripted installs keep a clean log). During
# downloads curl/wget draw their own byte-accurate bar; with_progress paints an
# indeterminate bar for steps whose duration is unknown.
if [ -t 1 ]; then
    PROGRESS_ON=1
else
    PROGRESS_ON=0
fi

BAR_WIDTH=40

# with_progress <label> <cmd> [args...]
# Runs <cmd...> while showing an animated, single-line progress bar. Returns
# the command's exit status. The animated line is erased before the caller
# continues so its own success/error message starts on a fresh line.
with_progress() {
    local label="$1"
    shift

    if [ "${PROGRESS_ON}" -ne 1 ]; then
        info "${label}..."
        "$@"
        return $?
    fi

    local -a spin=( '⠋' '⠙' '⠹' '⠸' '⠼' '⠴' '⠦' '⠧' '⠇' '⠏' )
    local cell=0 head=0 dir=1 i=0 pid ch
    local bar=""

    "$@" &
    pid=$!

    # Animation loop paints the bar on one line until the background command
    # exits; `set +e` is needed so a failing `wait` below doesn't abort.
    set +e
    while kill -0 "${pid}" 2>/dev/null; do
        bar=""
        for (( cell=0; cell<BAR_WIDTH; cell++ )); do
            if (( cell < head )); then
                ch="${C_GREEN}█${C_RESET}"
            elif (( cell == head )); then
                ch="${C_CYAN}▓${C_RESET}"
            else
                ch="░"
            fi
            bar+="${ch}"
        done
        printf "\r\033[K%s %s %s  %s" \
            "${C_BLUE}${C_BOLD}▸${C_RESET}" "${label}" "${bar}" "${spin[$(( i % ${#spin[@]} ))]}"
        sleep 0.1
        i=$(( i + 1 ))
        head=$(( head + dir ))
        if (( dir == 1 && head >= BAR_WIDTH - 1 )); then
            dir=-1
        elif (( dir == -1 && head <= 0 )); then
            dir=1
        fi
    done
    wait "${pid}"
    local rc=$?
    set -e
    printf "\r\033[K"
    return "${rc}"
}

# download <url> <output-path> [progress]
# Uses curl or wget (whichever is installed). Pass "progress" as the third
# argument to draw a live bar during the byte transfer; otherwise the download
# stays quiet so checksum sidecars and piped installs don't flash output.
download() {
    local url="$1" out="$2" mode="${3:-quiet}"
    if have_cmd curl; then
        if [ "${mode}" = "progress" ] && [ "${PROGRESS_ON}" -eq 1 ]; then
            curl -fSL --progress-bar "${url}" -o "${out}"
        else
            curl -fsSL "${url}" -o "${out}"
        fi
    elif have_cmd wget; then
        if [ "${mode}" = "progress" ] && [ "${PROGRESS_ON}" -eq 1 ]; then
            wget -q --show-progress -O "${out}" "${url}"
        else
            wget -q -O "${out}" "${url}"
        fi
    else
        fail "neither curl nor wget is installed; cannot download"
    fi
}

# http_exists <url> -> returns 0 if the URL responds 200, 1 otherwise
http_exists() {
    local url="$1"
    if have_cmd curl; then
        curl -fsSI "${url}" >/dev/null 2>&1
    else
        wget --spider -q "${url}" >/dev/null 2>&1
    fi
}

# verify_sha256 <dir> <artifact-name>
# Verifies <dir>/<artifact-name> against <dir>/<artifact-name>.sha256 (which
# the caller must have downloaded). Uses sha256sum (Linux coreutils) with a
# shasum fallback for macOS, where sha256sum is not installed by default.
verify_sha256() {
    local dir="$1" name="$2"
    (
        cd "${dir}"
        if have_cmd sha256sum; then
            sha256sum -c "${name}.sha256" >/dev/null
        elif have_cmd shasum; then
            shasum -a 256 -c "${name}.sha256" >/dev/null
        else
            fail "cannot verify checksum: neither sha256sum nor shasum is installed"
        fi
    )
}

# ---------------------------------------------------------------------------
# Platform detection
# ---------------------------------------------------------------------------
detect_os() {
    case "$(uname -s)" in
        Linux)  printf "linux" ;;
        Darwin) printf "darwin" ;;
        *) fail "unsupported operating system: $(uname -s)
  Phelix provides full support (binary + systemd) on Linux.
  On macOS only the binary can be installed (no systemd auto-start)." ;;
    esac
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64)  printf "amd64" ;;
        aarch64|arm64) printf "arm64" ;;
        armv7l)        printf "armv7" ;;
        armv6l)        printf "armv6" ;;
        *) fail "unsupported CPU architecture: $(uname -m)" ;;
    esac
}

# validate_version checks a release name before it is used in a download address.
validate_version() {
    local v="${1#v}"
    v="${v#V}"
    local pattern='^[0-9]+\.[0-9]+\.[0-9]+(-?[0-9A-Za-z][0-9A-Za-z-]*(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
    [[ "${v}" =~ ${pattern} ]] || fail "invalid release version: $1"
    printf '%s' "${v}"
}

# resolve_version reads the chosen channel or returns a requested release version.
resolve_version() {
    if [ "$1" != "latest" ]; then
        validate_version "$1"
        return
    fi
    local alias="latest" resolved=""
    [ "${CHANNEL}" != "beta" ] || alias="beta"
    local version_url="${BASE_URL}/releases/${alias}/version"
    if have_cmd curl; then
        resolved="$(curl -fsSI "${version_url}" 2>/dev/null \
            | awk 'tolower($1) == "x-phelix-version:" {sub(/^[^:]*:[ \t]*/, ""); sub(/\r$/, ""); print; exit}')" || resolved=""
    fi
    if [ -z "${resolved}" ]; then
        download "${version_url}" "${TMPDIR_WORK}/version" \
            || fail "could not find a ${CHANNEL} release"
        resolved="$(sed -n '1{s/\r$//;s/^[[:space:]]*//;s/[[:space:]]*$//;p;}' "${TMPDIR_WORK}/version")"
    fi
    resolved="$(validate_version "${resolved}")" || return 1
    local release="${resolved%%+*}"
    if [ "${CHANNEL}" = "stable" ]; then
        [[ "${release}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] \
            || fail "the stable channel reported a prerelease: ${resolved}"
    else
        [[ "${release}" =~ ^[0-9]+\.[0-9]+\.[0-9]+-?beta(\.[0-9A-Za-z-]+)*$ ]] \
            || fail "the beta channel reported an incompatible version: ${resolved}"
    fi
    printf '%s' "${resolved}"
}

# ---------------------------------------------------------------------------
# Uninstall
# ---------------------------------------------------------------------------
do_uninstall() {
    info "Uninstalling Phelix..."

    if [ "$(id -u)" -ne 0 ]; then
        warn "file removal needs root; continuing with sudo"
        SUDO="sudo"
    else
        SUDO=""
    fi

    # Stop + disable systemd unit if present (Linux only)
    if [ "${OS:-$(uname -s)}" = "linux" ] && have_cmd systemctl; then
        if ${SUDO} systemctl list-unit-files "${SERVICE_NAME}.service" >/dev/null 2>&1 \
           && ${SUDO} systemctl is-enabled "${SERVICE_NAME}.service" >/dev/null 2>&1; then
            info "Stopping and disabling ${SERVICE_NAME}.service..."
            ${SUDO} systemctl stop "${SERVICE_NAME}.service" 2>/dev/null || true
            ${SUDO} systemctl disable "${SERVICE_NAME}.service" 2>/dev/null || true
        fi
        ${SUDO} rm -f "/etc/systemd/system/${SERVICE_NAME}.service"
        ${SUDO} systemctl daemon-reload 2>/dev/null || true
    fi
    ${SUDO} rm -f "/etc/sudoers.d/${SERVICE_NAME}-restart"

    ${SUDO} rm -f "${INSTALL_DIR}/${SERVICE_NAME}"
    # Removes the legacy startup wrapper from older installs; the current
    # installer writes a direct unit and never creates this file.
    ${SUDO} rm -f "${INSTALL_DIR}/${SERVICE_NAME}-startup.sh"

    # Note: managed apps and ~/.phelix state are intentionally left in place.
    success "Phelix uninstalled."
    warn "App state under ~/.phelix/ was left untouched. Remove it manually if desired:"
    warn "  rm -rf ~/.phelix"
}

# ---------------------------------------------------------------------------
# Install
# ---------------------------------------------------------------------------
install_binary() {
    local os="$1" arch="$2"
    local binary_name="phelix-${os}-${arch}"
    local release_root="${BASE_URL}/releases/${VERSION}"
    local download_url="${release_root}/${binary_name}"

    info "Downloading Phelix ${C_CYAN}${VERSION}${C_RESET} for ${C_CYAN}${os}/${arch}${C_RESET}..."
    download "${download_url}" "${TMPDIR_WORK}/${binary_name}" progress

    # Optional SHA-256 verification. We fetch <binary>.sha256 only if it exists;
    # if it does, download it and verify; if not, warn and continue (no
    # half-baked check).
    local checksum_url="${download_url}.sha256"
    if http_exists "${checksum_url}"; then
        download "${checksum_url}" "${TMPDIR_WORK}/${binary_name}.sha256"
        with_progress "Verifying SHA-256 checksum" \
            verify_sha256 "${TMPDIR_WORK}" "${binary_name}"
        success "Checksum OK."
    else
        warn "No checksum published for this release; skipping verification."
    fi

    # Sanity-check the payload is non-empty.
    [ -s "${TMPDIR_WORK}/${binary_name}" ] || fail "downloaded binary is empty"

    local target="${INSTALL_DIR}/${SERVICE_NAME}"
    local SUDO=""

    if [ "$(id -u)" -ne 0 ]; then
            warn "installing to ${INSTALL_DIR} needs root; continuing with sudo"

            SUDO="sudo"

            # Authenticate BEFORE starting a background process.
            # This makes sudo ask for the password normally in the user's terminal.
            ${SUDO} -v || fail "sudo authentication failed"
    fi

     # Prepare the target directory.
    if [ -n "${SUDO}" ]; then
            ${SUDO} mkdir -p "${INSTALL_DIR}"
        else
            mkdir -p "${INSTALL_DIR}"
    fi

    # Install the binary.
    with_progress "Installing binary to ${target}" \
        bash -c "${SUDO} mkdir -p \"\$1\" && ${SUDO} install -m 0755 \"\$2\" \"\$3\"" \
        _ "${INSTALL_DIR}" "${TMPDIR_WORK}/${binary_name}" "${target}"

    success "Installed binary to ${C_CYAN}${target}${C_RESET}"
}

setup_systemd() {
    info "Configuring systemd service..."
    local SUDO=""
    [ "$(id -u)" -ne 0 ] && SUDO="sudo"

    # systemd unit. Runs as the invoking (non-root) user when installed via sudo.
    #
    # ExecStart runs `phelix controller` directly without a startup wrapper. The
    # service runs in the foreground: it loads local state, restores managed apps
    # that were previously running and optionally reports watched apps through
    # an authenticated TLS gRPC connection. systemd supervises the daemon
    # process itself, so `systemctl stop` delivers a clean SIGTERM.
    #
    # KillMode=process stops only the Controller process on stop/restart, keeping
    # managed application processes alive across Controller restarts.
    local svc_user="${SUDO_USER:-${USER}}"
    ${SUDO} tee "/etc/systemd/system/${SERVICE_NAME}.service" >/dev/null <<UNIT
[Unit]
Description=Phelix Controller
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${svc_user}
WorkingDirectory=${INSTALL_DIR}
ExecStart=${INSTALL_DIR}/phelix controller
Restart=always
RestartSec=5
TimeoutStopSec=30
KillMode=process

[Install]
WantedBy=multi-user.target
UNIT

    local systemctl_bin
    systemctl_bin="$(command -v systemctl || true)"
    if [ -n "${systemctl_bin}" ]; then
        local sudoers_tmp
        sudoers_tmp="$(mktemp)"
        printf '%s ALL=(root) NOPASSWD: %s restart --no-block %s.service\n' \
            "${svc_user}" "${systemctl_bin}" "${SERVICE_NAME}" > "${sudoers_tmp}"
        if command -v visudo >/dev/null 2>&1 && ! visudo -cf "${sudoers_tmp}" >/dev/null 2>&1; then
            warn "generated sudoers rule failed validation; remote Restart Monitor will be unsupported"
        else
            ${SUDO} install -m 0440 "${sudoers_tmp}" "/etc/sudoers.d/${SERVICE_NAME}-restart"
        fi
        rm -f "${sudoers_tmp}"
    fi

    ${SUDO} systemctl daemon-reload
    ${SUDO} systemctl enable "${SERVICE_NAME}.service"
    with_progress "Restarting ${SERVICE_NAME}.service" \
        ${SUDO} systemctl restart "${SERVICE_NAME}.service"

    # Verify the service actually came up as active. Poll briefly in case the
    # daemon takes a moment to start; on failure print useful diagnostics so a
    # broken install is debuggable instead of silent.
    local active_rc=0
    with_progress "Waiting for ${SERVICE_NAME}.service to become active" bash -c "
        deadline=15
        for _ in \$(seq 1 \"\${deadline}\"); do
            ${SUDO} systemctl is-active --quiet \"${SERVICE_NAME}.service\" && exit 0
            sleep 1
        done
        exit 1
    " || active_rc=$?

    if [ "${active_rc}" -eq 0 ]; then
        success "${SERVICE_NAME}.service is active (running)"
        return 0
    fi

    error "${SERVICE_NAME}.service is not active. Diagnostics:"
    ${SUDO} systemctl status "${SERVICE_NAME}.service" --no-pager || true
    error "Recent journal:"
    ${SUDO} journalctl -u "${SERVICE_NAME}.service" -n 50 --no-pager || true
    error "Process check:"
    ${SUDO} pgrep -af phelix || true
    fail "${SERVICE_NAME}.service failed to start. See diagnostics above."
}

# install_container prepares a fresh Container Mode installation without replacing existing state.
install_container() {
    [ "$OS" = linux ] || fail "Container Mode requires a Linux Docker host"
    case "$ARCH" in amd64|arm64) ;; *) fail "Controller images support linux/amd64 and linux/arm64 only" ;; esac
    need_cmd docker
    docker info >/dev/null || fail "Docker Engine is unavailable; install or start it before continuing"
    docker compose version >/dev/null || fail "Docker Compose is required"
    if have_cmd systemctl && { systemctl is-active --quiet phelix.service || systemctl is-enabled --quiet phelix.service; }; then
        fail "A host Controller is active or enabled. Follow the backup and migration guide before switching modes"
    fi
    local sudo_cmd="" operator_uid operator_gid
    operator_uid="$(id -u "${SUDO_USER:-$(id -un)}")"
    operator_gid="$(id -g "${SUDO_USER:-$(id -un)}")"
    if [ "$(id -u)" -ne 0 ]; then
        need_cmd sudo
        sudo -v || fail "sudo authentication failed"
        sudo_cmd=sudo
    fi
    local path
    for path in "$CONTROLLER_CONFIG_DIR" "$CONTROLLER_DATA_DIR" "$CONTROLLER_SOCKET_DIR" "$PROJECTS_DIR"; do
        [[ "$path" == /* && "$path" != / && "$path" != *$'\n'* && "$path" != *$'\r'* && "$path" != *'$'* && "$path" != *"'"* && "$path" != *'/../'* && "$path" != */.. ]] \
            || fail "Controller directories must be absolute paths without traversal, quotes, dollar signs or line breaks"
        [ ! -L "$path" ] || fail "Controller directories must not be symlinks: $path"
    done
    [[ "$CONTROLLER_NETWORK" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ ]] || fail "Invalid Docker network name"
    [ "$CONTROLLER_CONFIG_DIR" != "$CONTROLLER_DATA_DIR" ] && [ "$CONTROLLER_DATA_DIR" != "$CONTROLLER_SOCKET_DIR" ] && [ "$CONTROLLER_CONFIG_DIR" != "$CONTROLLER_SOCKET_DIR" ] && [ "$PROJECTS_DIR" != "$CONTROLLER_DATA_DIR" ] && [ "$PROJECTS_DIR" != "$CONTROLLER_SOCKET_DIR" ] && [ "$PROJECTS_DIR" != "$CONTROLLER_CONFIG_DIR" ] || fail "Configuration, state, socket and project directories must differ"
    local config_path="$CONTROLLER_CONFIG_DIR/controller.yaml" compose_path="$CONTROLLER_CONFIG_DIR/compose.yaml"
    if ${sudo_cmd} test -e "$config_path" || ${sudo_cmd} test -e "$compose_path"; then
        fail "Container installation already exists. Use the documented upgrade workflow; configuration was left unchanged"
    fi
    if ${sudo_cmd} test -d "$CONTROLLER_DATA_DIR"; then
        local contents
        contents="$(${sudo_cmd} ls -A "$CONTROLLER_DATA_DIR")"
        [ -z "$contents" ] || fail "Existing Controller state detected. Use the documented backup and migration workflow"
    fi
    local port
    for port in "${PROXY_PORTS[@]}"; do
        [[ "$port" =~ ^[0-9]{1,5}$ ]] && ((10#$port >= 1 && 10#$port <= 65535)) || fail "Proxy ports must be between 1 and 65535"
    done
    if [ "$WITH_CLI" -eq 1 ] && ${sudo_cmd} test -e /etc/phelix/client.yaml; then
        fail "Existing host CLI endpoint configuration was preserved; review /etc/phelix/client.yaml before installing"
    fi
    local image="ghcr.io/abdorrahmani/phelix:${VERSION//+/_}"
    docker pull "$image" || fail "Could not pull $image; confirm that this release has a validated Controller image"
    [ "$(docker run --rm "$image" version --short)" = "$VERSION" ] || fail "Image version does not match the selected release"
    ${sudo_cmd} install -d -m 0755 "$CONTROLLER_CONFIG_DIR"
    ${sudo_cmd} install -d -m 0700 "$CONTROLLER_DATA_DIR" "$CONTROLLER_SOCKET_DIR"
    if [ ! -d "$PROJECTS_DIR" ]; then
        ${sudo_cmd} install -d -m 0750 -o "$operator_uid" -g "$operator_gid" "$PROJECTS_DIR"
    fi
    cat > "$TMPDIR_WORK/controller.yaml" <<CONFIG
mode: container
network: $CONTROLLER_NETWORK
socket: /run/phelix/control.sock
socket_uid: $operator_uid
sources:
  - host: '$PROJECTS_DIR'
    controller: /projects
CONFIG
    cat > "$TMPDIR_WORK/compose.yaml" <<COMPOSE
name: phelix
services:
  phelix:
    image: $image
    restart: unless-stopped
    stop_grace_period: 120s
    environment:
      PHELIX_CONTROLLER_CONFIG: /etc/phelix/controller.yaml
      PHELIX_CONTROL_SOCKET: /run/phelix/control.sock
    volumes:
      - type: bind
        source: '$CONTROLLER_DATA_DIR'
        target: /var/lib/phelix
      - type: bind
        source: '$CONTROLLER_SOCKET_DIR'
        target: /run/phelix
      - type: bind
        source: '$config_path'
        target: /etc/phelix/controller.yaml
        read_only: true
      - type: bind
        source: '$PROJECTS_DIR'
        target: /projects
        read_only: true
      - type: bind
        source: /var/run/docker.sock
        target: /var/run/docker.sock
    networks: [applications]
COMPOSE
    if [ "${#PROXY_PORTS[@]}" -gt 0 ]; then
        printf '    ports:\n' >> "$TMPDIR_WORK/compose.yaml"
        for port in "${PROXY_PORTS[@]}"; do
            printf '      - "127.0.0.1:%s:%s"\n' "$port" "$port" >> "$TMPDIR_WORK/compose.yaml"
        done
    fi
    cat >> "$TMPDIR_WORK/compose.yaml" <<COMPOSE
networks:
  applications:
    name: $CONTROLLER_NETWORK
COMPOSE
    docker compose -f "$TMPDIR_WORK/compose.yaml" config --quiet
    ${sudo_cmd} install -m 0600 "$TMPDIR_WORK/controller.yaml" "$config_path"
    ${sudo_cmd} install -m 0644 "$TMPDIR_WORK/compose.yaml" "$compose_path"
    if ! docker compose -f "$compose_path" up -d --wait --wait-timeout 90; then
        docker compose -f "$compose_path" logs --tail=50 phelix >&2
        fail "Controller did not become ready. State and configuration were retained for diagnosis"
    fi
    docker compose -f "$compose_path" exec -T phelix phelix controller health
    docker compose -f "$compose_path" exec -T phelix phelix controller info
    if [ "$WITH_CLI" -eq 1 ]; then
        install_binary "$OS" "$ARCH"
        printf "socket: '%s/control.sock'\n" "$CONTROLLER_SOCKET_DIR" > "$TMPDIR_WORK/client.yaml"
        ${sudo_cmd} install -d -m 0755 /etc/phelix
        if ${sudo_cmd} test -e /etc/phelix/client.yaml; then
            fail "Existing host CLI endpoint configuration was preserved; review /etc/phelix/client.yaml before connecting"
        fi
        ${sudo_cmd} install -m 0644 "$TMPDIR_WORK/client.yaml" /etc/phelix/client.yaml
        "${INSTALL_DIR}/phelix" controller health
    fi
    success "Container Controller is ready. Projects: $PROJECTS_DIR"
    info "Compose file: $compose_path"
    info "Prepare phelix.yaml, Dockerfile and .dockerignore before using the read-only project mount"
    info "Inside a project: docker compose -f '$compose_path' exec -T -w /projects/my-app phelix phelix build my-app"
    info "Publish app proxy ports with --proxy-port during installation, or edit the Compose file and recreate only the Controller"
}

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------
parse_args() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --docker) DOCKER_MODE=1; shift ;;
            --with-cli) WITH_CLI=1; shift ;;
            --config-dir) CONTROLLER_CONFIG_DIR="${2:?--config-dir requires a path}"; shift 2 ;;
            --data-dir) CONTROLLER_DATA_DIR="${2:?--data-dir requires a path}"; shift 2 ;;
            --socket-dir) CONTROLLER_SOCKET_DIR="${2:?--socket-dir requires a path}"; shift 2 ;;
            --projects-dir) PROJECTS_DIR="${2:?--projects-dir requires a path}"; shift 2 ;;
            --network) CONTROLLER_NETWORK="${2:?--network requires a name}"; shift 2 ;;
            --proxy-port) PROXY_PORTS+=("${2:?--proxy-port requires a port}"); shift 2 ;;
            --channel)
                CHANNEL="${2:?--channel requires a value (stable or beta)}"
                shift 2
                ;;
            --version)
                VERSION="${2:?--version requires a value (e.g. 1.2.3)}"
                shift 2
                ;;
            --install-dir)
                INSTALL_DIR="${2:?--install-dir requires a path}"
                shift 2
                ;;
            --no-service)
                NO_SERVICE=1
                shift
                ;;
            --uninstall)
                UNINSTALL=1
                shift
                ;;
            --help|-h)
                cat <<'HELP'
Phelix installer

Usage:
  curl -fsSL https://phelix.anophel.com/install.sh | bash

Options (pass after `bash -s --`):
  --channel <c>      Release channel: stable or beta. Default: stable.
  --version <v>      Pin a release version (e.g. 1.2.3). Default: latest.
  --install-dir <p>  Binary install directory. Default: /usr/local/bin.
  --no-service       Do not create/enable the systemd service.
  --docker           Pull and start the official Container Controller.
  --with-cli         Also install the host CLI in Container Mode.
  --config-dir <p>   Container configuration directory. Default: /etc/phelix.
  --data-dir <p>     Persistent container state. Default: /var/lib/phelix.
  --socket-dir <p>   Shared socket directory. Default: /var/run/phelix.
  --projects-dir <p> Project source root. Default: /srv/phelix/projects.
  --network <n>     Shared Docker network. Default: phelix.
  --proxy-port <p>  Publish a proxy port on host loopback. May be repeated.
  --uninstall        Remove Phelix (leaves ~/.phelix state in place).
  --help             Show this help.

Environment:
  PHELIX_CHANNEL     Same as --channel.
  PHELIX_VERSION     Same as --version.
  PHELIX_RELEASE_BASE_URL  Release server address. Default: https://phelix.anophel.com.
HELP
                exit 0
                ;;
            *)
                fail "unknown option: $1 (try --help)"
                ;;
        esac
    done
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
    parse_args "$@"
    [ "$DOCKER_MODE" -eq 0 ] || [ "$UNINSTALL" -eq 0 ] || fail "Use Compose and Phelix remove for Container Mode cleanup"

    need_cmd uname
    need_cmd install   # coreutils; also provided by mktemp above

    OS="$(detect_os)"
    ARCH="$(detect_arch)"

    if [ "${UNINSTALL}" -eq 1 ]; then
        do_uninstall
        exit 0
    fi

    case "${CHANNEL}" in
        stable|beta) ;;
        *) fail "unknown release channel: ${CHANNEL}; choose stable or beta" ;;
    esac
    BASE_URL="${BASE_URL%/}"
    local https_url='^https://[^/?#[:space:]]+(/[^?#[:space:]]*)?$'
    local local_url='^http://(localhost|127(\.[0-9]{1,3}){3}|\[::1\])(:[0-9]+)?(/[^?#[:space:]]*)?$'
    [[ "${BASE_URL}" =~ ${https_url} || "${BASE_URL}" =~ ${local_url} ]] \
        || fail "PHELIX_RELEASE_BASE_URL must use HTTPS, or HTTP on a local address"

    info "This is the ${C_BOLD}Phelix${C_RESET} installer."
    info "Detected platform: ${C_CYAN}${OS}/${ARCH}${C_RESET}"
    echo
    printf "${C_CYAN}"
    cat <<'BANNER'
██████╗ ██╗  ██╗███████╗██╗     ██╗██╗  ██╗
██╔══██╗██║  ██║██╔════╝██║     ██║╚██╗██╔╝
██████╔╝███████║█████╗  ██║     ██║ ╚███╔╝
██╔═══╝ ██╔══██║██╔══╝  ██║     ██║ ██╔██╗
██║     ██║  ██╗███████╗███████╗██║██╔╝ ██╗
╚═╝     ╚═╝  ╚═╝╚══════╝╚══════╝╚═╝╚═╝  ╚═╝
BANNER
    printf "${C_RESET}"
    echo

    VERSION="$(resolve_version "${VERSION}")"

    if [ "$DOCKER_MODE" -eq 1 ]; then
        [ "$UNINSTALL" -eq 0 ] || fail "Use Compose and Phelix remove for Container Mode cleanup"
        install_container
        return
    fi
    [ "$WITH_CLI" -eq 0 ] || fail "--with-cli requires --docker"
    install_binary "${OS}" "${ARCH}"

    # systemd auto-start is Linux-only.
    if [ "${OS}" = "linux" ] && [ "${NO_SERVICE}" -eq 0 ]; then
        if have_cmd systemctl; then
            setup_systemd
        else
            warn "systemctl not found; skipping service setup."
            warn "Run 'phelix controller' in the foreground, or create your own init unit."
        fi
    elif [ "${OS}" = "darwin" ]; then
        info "macOS: binary installed. The Controller requires a Linux host."
    fi

    # Verify the binary is runnable.
    if command -v phelix >/dev/null 2>&1; then
        success "Phelix installed:"
        phelix version || true
    else
        warn "'phelix' is not on your current PATH. It was installed to:"
        warn "  ${C_CYAN}${INSTALL_DIR}/phelix${C_RESET}"
        warn "Add ${INSTALL_DIR} to your PATH or invoke it directly."
    fi

    echo
    success "Done. Next steps:"
    echo "  ${C_CYAN}phelix auth login${C_RESET}            # authenticate"
    echo "  ${C_CYAN}phelix build myapp --port 8080${C_RESET}  # build & run an app"
    [ "${OS}" = "linux" ] && [ "${NO_SERVICE}" -eq 0 ] && \
        echo "  ${C_CYAN}sudo systemctl status phelix${C_RESET}   # Controller service"
    # On servers used only for monitoring, prefer the agent-scoped session: a
    # stolen token from a compromised server then has no account access.
    echo
    echo "  For backend monitoring, authenticate with"
    echo "  ${C_CYAN}phelix auth login --scope agent${C_RESET}, the issued token"
    echo "  only authorizes monitoring, not your account."
    echo "  ${C_CYAN}phelix watch myapp${C_RESET}         # enable backend reporting for this app"
}

main "$@"
