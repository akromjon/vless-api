#!/usr/bin/env bash
set -Eeuo pipefail

target_os="${GOOS:-linux}"
target_arch="${GOARCH:-amd64}"
output="${OUTPUT:-vless-api-${target_os}-${target_arch}}"
host_os="$(go env GOHOSTOS)"
host_arch="$(go env GOHOSTARCH)"

GOOS="${host_os}" GOARCH="${host_arch}" go test ./...
CGO_ENABLED=0 GOOS="${target_os}" GOARCH="${target_arch}" \
	go build -trimpath -ldflags="-s -w" -o "${output}" .

echo "Built ${output}"
