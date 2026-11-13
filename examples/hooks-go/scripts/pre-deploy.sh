#!/bin/sh
set -eu
mkdir -p .phelix-example
date +%s > .phelix-example/last-migration
echo "pre_deploy: migration marker written"
