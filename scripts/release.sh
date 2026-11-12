#!/usr/bin/env bash
# 未指定版本时建议下一个 patch 版本并要求确认；只有 --push 才推送 tag
set -euo pipefail
cd "$(dirname "$0")/.."
version=
push=false
stable_version='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
for arg do
    case "$arg" in
        --push)
            if $push; then
                echo 'Duplicate --push option.' >&2
                exit 1
            fi
            push=true ;;
        *)
            if [[ -n $version || ! $arg =~ $stable_version ]]; then
                echo 'usage: scripts/release.sh [vX.Y.Z] [--push]' >&2
                exit 1
            fi
            version=$arg ;;
    esac
done
if [[ -n $(git status --porcelain) ]]; then
    echo 'Commit or stash all changes before releasing.' >&2
    exit 1
fi
git rev-parse --verify HEAD >/dev/null
if $push; then
    git remote get-url origin >/dev/null
fi
if [[ -z $version ]]; then
    suggested=v0.1.0
    tags=$(git tag --sort=-version:refname)
    while IFS= read -r tag; do
        if [[ $tag =~ $stable_version ]]; then
            suggested="${tag%.*}.$((${tag##*.} + 1))"
            break
        fi
    done <<< "$tags"
    printf 'Release version [%s]: ' "$suggested"
    if ! IFS= read -r version; then
        echo 'No version input; no tag created.' >&2
        exit 1
    fi
    version=${version:-$suggested}
    if [[ ! $version =~ $stable_version ]]; then
        echo "Invalid stable version: $version (expected vX.Y.Z)." >&2
        exit 1
    fi
    printf 'Create tag %s' "$version"
    if $push; then
        printf ' and push it to origin'
    fi
    printf '? [y/N] '
    if ! IFS= read -r answer; then
        echo 'No confirmation; no tag created.' >&2
        exit 1
    fi
    case "$answer" in
        y|Y|yes|YES) ;;
        *) echo 'Canceled; no tag created.'; exit 0 ;;
    esac
fi
if git show-ref --verify --quiet "refs/tags/$version"; then
    echo "Tag $version already exists; tags must not be reused." >&2
    exit 1
fi
make check test test-scripts
git tag -a "$version" -m "Release $version"
if $push; then
    git push origin "refs/tags/$version"
else
    printf 'Tag created. Publish with: git push origin refs/tags/%s\n' "$version"
fi
