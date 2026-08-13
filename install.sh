#!/usr/bin/env bash
#
# install.sh — Phelix one-line installer (hosted at phelix.anophel.com/install.sh)
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
#   5. On Linux: registers + enables a phelix.service systemd unit that starts
#      the background gRPC monitor daemon and all managed apps on boot.
#      On macOS: installs the binary only (no systemd).
#
# This is the end-user counterpart of the in-repo setup.sh, which builds from
# source for developers instead of downloading a prebuilt binary.
#
# Usage:
#   curl -fsSL https://phelix.anophel.com/install.sh | bash           # latest
#   curl -fsSL https://phelix.anophel.com/install.sh | bash -s -- --version v1.2.3
#   PHELIX_VERSION=v1.2.3 curl -fsSL .../install.sh | bash
#   curl -fsSL .../install.sh | bash -s -- --uninstall
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
BASE_URL="https://phelix.anophel.com"
VERSION="${PHELIX_VERSION:-latest}"
INSTALL_DIR="/usr/local/bin"
SERVICE_NAME="phelix"
NO_SERVICE=0
UNINSTALL=0

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

# download <url> <output-path>
download() {
    local url="$1" out="$2"
    if have_cmd curl; then
        curl -fsSL "${url}" -o "${out}"
    elif have_cmd wget; then
        wget -q --show-progress=off -O "${out}" "${url}"
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

# Resolve "latest" to a concrete version by following the stable URL. Returns
# the concrete version string, or "latest" if resolution is unavailable (the
# download itself will still work against the /latest/ path).
resolve_version() {
    local v="${1}"
    if [ "${v}" != "latest" ]; then
        printf "%s" "${v}"
        return
    fi
    if have_cmd curl; then
        local resolved
        if resolved="$(curl -fsSI "${BASE_URL}/releases/latest/version" 2>/dev/null \
                       | sed -n 's/^[Xx]-Phelix-[Vv]ersion:[[:space:]]*//p' \
                       | tr -d '[:space:]\r' | head -n1)" && [ -n "${resolved}" ]; then
            printf "%s" "${resolved}"
            return
        fi
    fi
    printf "latest"
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

    ${SUDO} rm -f "${INSTALL_DIR}/${SERVICE_NAME}"
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
    download "${download_url}" "${TMPDIR_WORK}/${binary_name}"

    # Optional SHA-256 verification. We fetch <binary>.sha256 only if it exists;
    # if it does, verify; if not, warn and continue (no half-baked check).
    local checksum_url="${download_url}.sha256"
    if http_exists "${checksum_url}"; then
        info "Verifying SHA-256 checksum..."
        download "${checksum_url}" "${TMPDIR_WORK}/${binary_name}.sha256"
        need_cmd sha256sum
        # checksum file is expected as: "<hex>  <filename>"
        ( cd "${TMPDIR_WORK}" && sha256sum -c "${binary_name}.sha256" >/dev/null ) \
            || fail "checksum verification failed for ${binary_name}"
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
    fi

    ${SUDO} mkdir -p "${INSTALL_DIR}"
    ${SUDO} install -m 0755 "${TMPDIR_WORK}/${binary_name}" "${target}"
    success "Installed binary to ${C_CYAN}${target}${C_RESET}"
}

setup_systemd() {
    local start_script="${INSTALL_DIR}/${SERVICE_NAME}-startup.sh"

    info "Configuring systemd service..."
    local SUDO=""
    [ "$(id -u)" -ne 0 ] && SUDO="sudo"

    # Startup wrapper: start the monitor daemon, give it a moment, then start
    # any managed apps (mirrors the in-repo setup.sh).
    ${SUDO} tee "${start_script}" >/dev/null <<'STARTUP'
#!/usr/bin/env bash
# Created by the Phelix installer. Starts the background monitor and all
# managed applications on boot / service (re)start.
set -e
/usr/local/bin/phelix monitor &
sleep 5
/usr/local/bin/phelix start
STARTUP
    ${SUDO} chmod 0755 "${start_script}"

    # systemd unit. Runs as the invoking (non-root) user when installed via sudo.
    local svc_user="${SUDO_USER:-${USER}}"
    ${SUDO} tee "/etc/systemd/system/${SERVICE_NAME}.service" >/dev/null <<UNIT
[Unit]
Description=Phelix gRPC Monitoring Service
After=network.target

[Service]
Type=simple
User=${svc_user}
WorkingDirectory=${INSTALL_DIR}
ExecStart=${start_script}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT

    ${SUDO} systemctl daemon-reload
    ${SUDO} systemctl enable "${SERVICE_NAME}.service"
    ${SUDO} systemctl restart "${SERVICE_NAME}.service"
    success "Enabled and started ${C_CYAN}${SERVICE_NAME}.service${C_RESET}"
}

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------
parse_args() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --version)
                VERSION="${2:?--version requires a value (e.g. v1.2.3)}"
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
  --version <v>      Pin a release version (e.g. v1.2.3). Default: latest.
  --install-dir <p>  Binary install directory. Default: /usr/local/bin.
  --no-service       Do not create/enable the systemd service.
  --uninstall        Remove Phelix (leaves ~/.phelix state in place).
  --help             Show this help.

Environment:
  PHELIX_VERSION     Same as --version.
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

    need_cmd uname
    need_cmd install   # coreutils; also provided by mktemp above

    OS="$(detect_os)"
    ARCH="$(detect_arch)"

    if [ "${UNINSTALL}" -eq 1 ]; then
        do_uninstall
        exit 0
    fi

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

    install_binary "${OS}" "${ARCH}"

    # systemd auto-start is Linux-only.
    if [ "${OS}" = "linux" ] && [ "${NO_SERVICE}" -eq 0 ]; then
        if have_cmd systemctl; then
            setup_systemd
        else
            warn "systemctl not found; skipping service setup."
            warn "Run 'phelix monitor &' manually, or create your own init unit."
        fi
    elif [ "${OS}" = "darwin" ]; then
        info "macOS: binary installed. No systemd on macOS — start the monitor manually:"
        info "  ${C_CYAN}phelix monitor &${C_RESET}"
        info "Or use launchd / a background-session script for auto-start."
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
        echo "  ${C_CYAN}sudo systemctl status phelix${C_RESET}   # monitor service"
}

main "$@"
