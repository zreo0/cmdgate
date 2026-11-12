#!/usr/bin/env bash
# 在临时仓库和下载夹具中验证发布、版本解析及安装失败边界
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# 所有预期失败都必须有非零状态，避免脚本静默跳过验证
expect_failure() {
    if "$@" > "$work/failure.log" 2>&1; then
        echo "Expected failure: $*" >&2
        exit 1
    fi
}

mkdir -p "$work/repo/scripts" "$work/home"
export HOME="$work/home" GIT_CONFIG_NOSYSTEM=1
# 避免用户的签名设置和 Git hook 干扰临时仓库
export GIT_CONFIG_GLOBAL=/dev/null
cp "$root/scripts/release.sh" "$root/scripts/release-notes.sh" "$work/repo/scripts/"
git init -q "$work/repo"
cd "$work/repo"
git config user.name 'Release Test'
git config user.email 'release@example.invalid'
printf '.PHONY: check test test-scripts\ncheck test test-scripts:\n\t@true\n' > Makefile
git add .
git commit -qm 'feat: first release'
# 首次建议、默认拒绝和 EOF 都不能意外创建 tag
printf '\n\n' | ./scripts/release.sh > "$work/prompt"
grep -Fq 'Release version [v0.1.0]' "$work/prompt"
expect_failure ./scripts/release.sh < /dev/null
printf '\n' > "$work/no-confirmation"
expect_failure ./scripts/release.sh < "$work/no-confirmation"
printf 'v01.0.0\ny\n' > "$work/invalid-version"
expect_failure ./scripts/release.sh < "$work/invalid-version"
test -z "$(git tag)"
expect_failure ./scripts/release.sh v01.0.0
expect_failure ./scripts/release.sh v1.0.0 unexpected
touch dirty
expect_failure ./scripts/release.sh v1.0.0
rm dirty
# 验证失败时不能留下版本 tag
printf '\ncheck:\n\t@false\n' >> Makefile
git add Makefile
git commit -qm 'test: reject failed checks'
expect_failure ./scripts/release.sh v1.0.0
if git show-ref --verify --quiet refs/tags/v1.0.0; then
    echo 'Failed checks created a tag.' >&2
    exit 1
fi
git checkout HEAD^ -- Makefile
git commit -qm 'test: restore passing checks'
./scripts/release.sh v1.0.0
./scripts/release-notes.sh v1.0.0 > "$work/first-notes"
grep -q 'feat: first release' "$work/first-notes"
expect_failure ./scripts/release.sh v1.0.0
printf 'next\n' > change
git add change
git commit -qm 'fix: second release'
git init -q --bare "$work/origin.git"
git remote add origin "$work/origin.git"
./scripts/release.sh v1.0.1 --push
git --git-dir="$work/origin.git" show-ref --verify refs/tags/v1.0.1 >/dev/null
./scripts/release-notes.sh v1.0.1 > "$work/notes"
grep -q 'fix: second release' "$work/notes"
if grep -q 'feat: first release' "$work/notes"; then
    echo 'Release notes included an earlier release.' >&2
    exit 1
fi

# 版本按数值排序，跳过预发布 tag；默认接受、现场改版及交互推送都受确认保护
git tag v1.9.9
git tag v1.10.9
git tag v99.0.0-rc.1
printf '\ny\n' | ./scripts/release.sh > "$work/prompt"
grep -Fq 'Release version [v1.10.10]' "$work/prompt"
git show-ref --verify refs/tags/v1.10.10 >/dev/null
printf 'v2.0.0\nno\n' | ./scripts/release.sh --push
if git show-ref --verify --quiet refs/tags/v2.0.0; then
    echo 'Canceled release created a tag.' >&2
    exit 1
fi
printf 'v2.0.0\ny\n' | ./scripts/release.sh --push
git --git-dir="$work/origin.git" show-ref --verify refs/tags/v2.0.0 >/dev/null
printf 'v2.0.0\ny\n' > "$work/duplicate-version"
expect_failure ./scripts/release.sh < "$work/duplicate-version"

# 只替换下载和平台探测；使用真实 tar、SHA-256 和文件安装
mkdir -p "$work/tools" "$work/assets" "$work/payload"
export RELEASE_FIXTURE="$work"
cat > "$work/tools/curl" <<'SH'
#!/bin/sh
set -eu
out=
for arg do
    if [ "${previous:-}" = --output ]; then out=$arg; fi
    previous=$arg
    url=$arg
done
printf '%s\n' "$url" >> "$RELEASE_FIXTURE/urls"
case "$url" in
    https://github.com/test/cmdgate/releases/latest)
        printf '%s' 'https://github.com/test/cmdgate/releases/tag/v1.0.1' ;;
    https://github.com/test/cmdgate/releases/download/v1.0.1/*)
        cp "$RELEASE_FIXTURE/assets/${url##*/}" "$out" ;;
    *) exit 22 ;;
esac
SH
cat > "$work/tools/uname" <<'SH'
#!/bin/sh
case "$1" in
    -s) echo "$TEST_OS" ;;
    -m) echo "$TEST_ARCH" ;;
esac
SH
cat > "$work/tools/sudo" <<'SH'
#!/bin/sh
exec "$@"
SH
cat > "$work/payload/cmdgate" <<'SH'
#!/bin/sh
printf '%s\n' "$*" > "$RELEASE_FIXTURE/executed"
SH
chmod +x "$work/tools/"* "$work/payload/cmdgate"
export PATH="$work/tools:$PATH"
for target in darwin_arm64 darwin_amd64 linux_arm64 linux_amd64; do
    tar -czf "$work/assets/cmdgate_v1.0.1_$target.tar.gz" -C "$work/payload" cmdgate
done
(
    cd "$work/assets"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum ./*.tar.gz | sed 's|  ./|  |' > SHA256SUMS
    else
        shasum -a 256 ./*.tar.gz | sed 's|  ./|  |' > SHA256SUMS
    fi
)
for target in Darwin:arm64 Darwin:x86_64 Linux:aarch64 Linux:x86_64; do
    export TEST_OS=${target%:*} TEST_ARCH=${target#*:}
    "$root/scripts/install.sh" --repo test/cmdgate --client-dir "$work/client"
    cmp "$work/payload/cmdgate" "$work/client/cmdgate"
    test -x "$work/client/cmdgate"
done
"$root/scripts/install.sh" --repo test/cmdgate --version v1.0.1
grep -qx 'install --mode local' "$work/executed"
rm "$work/executed"
mkdir -p "$work/bad-client/cmdgate"
expect_failure "$root/scripts/install.sh" --repo test/cmdgate --client-dir "$work/bad-client"
expect_failure "$root/scripts/install.sh" --repo test/cmdgate --version ../bad
expect_failure env TEST_OS=Windows "$root/scripts/install.sh" --repo test/cmdgate
expect_failure env TEST_ARCH=riscv64 "$root/scripts/install.sh" --repo test/cmdgate
cp "$work/assets/SHA256SUMS" "$work/good-sums"
: > "$work/assets/SHA256SUMS"
expect_failure "$root/scripts/install.sh" --repo test/cmdgate
cat "$work/good-sums" "$work/good-sums" > "$work/assets/SHA256SUMS"
expect_failure "$root/scripts/install.sh" --repo test/cmdgate
cp "$work/good-sums" "$work/assets/SHA256SUMS"
printf 'corrupted' >> "$work/assets/cmdgate_v1.0.1_linux_amd64.tar.gz"
expect_failure "$root/scripts/install.sh" --repo test/cmdgate --client-dir "$work/client"
cmp "$work/payload/cmdgate" "$work/client/cmdgate"
expect_failure "$root/scripts/install.sh" --repo test/cmdgate
rm "$work/assets/SHA256SUMS"
expect_failure "$root/scripts/install.sh" --repo test/cmdgate
if [ -e "$work/executed" ]; then
    echo 'An invalid download was executed.' >&2
    exit 1
fi
echo 'Release and installer tests passed.'
