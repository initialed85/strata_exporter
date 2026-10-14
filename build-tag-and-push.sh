#!/usr/bin/env bash

set -e -m -x

# this machine is arm64; the cluster nodes are amd64, so build on the x86 daemon
# Set DOCKER_HOST to a remote daemon if you build somewhere other than the local docker context.
export DOCKER_HOST="${DOCKER_HOST:-}"

cd "$(dirname -- "${BASH_SOURCE[0]}")"

GOOS=linux GOARCH=amd64 go build -v -o entrypoint ./cmd/strata_exporter

docker build --platform linux/amd64 -f docker/Dockerfile -t initialed85/strata-exporter:latest .
docker image push initialed85/strata-exporter:latest
