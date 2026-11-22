#!/usr/bin/env bash
set -euo pipefail
umask 077
cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")"
case "$PWD" in
  *$'\n'*|*$'\r'*) printf 'The example directory must not contain line breaks.\n' >&2; exit 1 ;;
esac
yaml_directory=${PWD//\'/\'\'}
mkdir -p -- run
cat > controller.yaml <<EOF
mode: container
network: phelix-container-example
socket: /run/phelix/control.sock
socket_uid: $(id -u)
sources:
  - host: '${yaml_directory}/app'
    controller: /workspace/app
EOF
if [[ ! -e .env ]]; then
  password=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
  printf 'POSTGRES_PASSWORD=%s\n' "$password" > .env
fi

image="${PHELIX_IMAGE:-}"
if [[ -z "$image" ]]; then
  version="${1:-}"
  if [[ -z "$version" && -f VERSION ]]; then version="$(cat VERSION)"; fi
  version="${version#v}"
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$ ]] || { printf 'Pass a published version to setup.sh, or set PHELIX_IMAGE.\n' >&2; exit 1; }
  image="ghcr.io/abdorrahmani/phelix:${version//+/_}"
fi
[[ "$image" =~ ^[0-9A-Za-z./:_@+-]+$ ]] || { printf 'Invalid Controller image reference.\n' >&2; exit 1; }
found=0
while IFS= read -r line; do
  if [[ "$line" == PHELIX_IMAGE=* ]]; then found=1; break; fi
done < .env
if [[ "$found" == 0 ]]; then printf 'PHELIX_IMAGE=%s\n' "$image" >> .env; fi
