#!/usr/bin/env bash

set -e -m -x

# this machine is arm64; the cluster nodes are amd64, so build on the x86 daemon
export DOCKER_HOST="${DOCKER_HOST:-tcp://192.168.11.128:2375}"

cd "$(dirname -- "${BASH_SOURCE[0]}")"

GOOS=linux GOARCH=amd64 go build -v -o entrypoint ./cmd/strata_exporter

docker build --platform linux/amd64 -f docker/Dockerfile -t initialed85/strata-exporter:latest .
docker image push initialed85/strata-exporter:latest
