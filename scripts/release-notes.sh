#!/usr/bin/env bash
# 从上一个可达版本到当前 tag 的提交生成 Release 正文，首版包含全部提交
set -euo pipefail
cd "$(dirname "$0")/.."
tag=${1:?usage: scripts/release-notes.sh vX.Y.Z}
commit=$(git rev-parse --verify "refs/tags/$tag^{commit}")
previous=$(git describe --tags --match 'v[0-9]*' --abbrev=0 "$commit^" 2>/dev/null || true)
if [[ -n $previous ]]; then
    git log --format='- %s (%h)' "$previous..$commit"
else
    git log --format='- %s (%h)' "$commit"
fi
