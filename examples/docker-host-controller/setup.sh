#!/usr/bin/env bash
set -euo pipefail
umask 077
cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")"
case "$PWD" in
  *$'\n'*|*$'\r'*) printf 'The example directory must not contain line breaks.\n' >&2; exit 1 ;;
esac
yaml_directory=${PWD//\'/\'\'}
mkdir -p -- state
cat > controller.yaml <<EOF
mode: host
socket: '${yaml_directory}/state/control.sock'
sources:
  - host: '${yaml_directory}/app'
    controller: '${yaml_directory}/app'
EOF
if [[ ! -e .env ]]; then
  password=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
  printf 'POSTGRES_PASSWORD=%s\n' "$password" > .env
fi
