#!/bin/sh
# 在一次性的 Linux 容器里以 root 运行隔离验收测试
# 测试会创建系统用户并安装到 /opt/cmdgate，所以不能直接在本机运行
set -eu
cd "$(dirname "$0")/.."
exec docker run --rm \
    -v "$PWD":/src:ro \
    -v cmdgate-gomod:/go/pkg/mod \
    -w /src \
    -e GOFLAGS=-buildvcs=false \
    golang:1.27 \
    go test -tags isolation -run 'TestIsolation' -count=1 -v ./e2e/
