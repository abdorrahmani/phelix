#!/bin/sh
set -eu
go vet ./...
echo "pre_build: source checks passed"
