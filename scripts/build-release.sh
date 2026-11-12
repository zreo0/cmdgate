#!/usr/bin/env bash
# 为指定稳定版本构建四个平台的归档及校验和，输出到 dist/<tag>
set -euo pipefail
cd "$(dirname "$0")/.."

version=${1:-}
if [[ ! $version =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
    echo 'usage: scripts/build-release.sh vX.Y.Z' >&2
    exit 1
fi
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
mkdir -p "dist/$version"
for target in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do
    os=${target%/*}
    arch=${target#*/}
    archive="cmdgate_${version}_${os}_${arch}.tar.gz"
    echo "building $os/$arch"
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
        -ldflags "-s -w -X cmdgate/internal/cli.Version=$version" \
        -o "$stage/cmdgate" ./cmd/cmdgate
    cp LICENSE README.md README.zh-CN.md "$stage/"
    tar -czf "dist/$version/$archive" -C "$stage" cmdgate LICENSE README.md README.zh-CN.md
done
(
    cd "dist/$version"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum cmdgate_"$version"_*.tar.gz > SHA256SUMS
    else
        shasum -a 256 cmdgate_"$version"_*.tar.gz > SHA256SUMS
    fi
)
echo "assets: dist/$version"
