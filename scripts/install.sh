#!/bin/sh
# 下载同一版本的归档和校验和，校验成功后安装客户端或独立用户服务
set -eu

repo=${CMDGATE_REPO:-zreo0/cmdgate}
version=${CMDGATE_VERSION:-latest}
client_dir=
while [ "$#" -gt 0 ]; do
    case "$1" in
        --repo|--version|--client-dir)
            [ "$#" -ge 2 ] || { echo "Missing value for $1" >&2; exit 1; }
            case "$1" in
                --repo) repo=$2 ;;
                --version) version=$2 ;;
                --client-dir) client_dir=$2 ;;
            esac
            shift 2 ;;
        *) echo 'usage: install.sh --repo OWNER/REPO [--version vX.Y.Z] [--client-dir DIR]' >&2; exit 1 ;;
    esac
done
if ! printf '%s\n' "$repo" | LC_ALL=C grep -Eq '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$'; then
    echo 'Set --repo OWNER/REPO (or CMDGATE_REPO) to the GitHub repository.' >&2
    exit 1
fi
case $(uname -s) in
    Darwin) os=darwin ;;
    Linux) os=linux ;;
    *) echo 'Supported systems: macOS and Linux.' >&2; exit 1 ;;
esac
case $(uname -m) in
    arm64|aarch64) arch=arm64 ;;
    x86_64|amd64) arch=amd64 ;;
    *) echo 'Supported architectures: arm64 and amd64.' >&2; exit 1 ;;
esac
printf 'Preparing cmdgate installation for %s/%s...\n' "$os" "$arch" >&2
for tool in curl tar mktemp; do
    command -v "$tool" >/dev/null 2>&1 || { echo "Missing tool: $tool" >&2; exit 1; }
done
if command -v sha256sum >/dev/null 2>&1; then
    checksum=sha256sum
elif command -v shasum >/dev/null 2>&1; then
    checksum=shasum
else
    echo 'Install sha256sum or shasum first.' >&2
    exit 1
fi
if [ "$version" = latest ]; then
    printf 'Resolving the latest stable release...\n' >&2
    url=$(curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
        --output /dev/null --write-out '%{url_effective}' "https://github.com/$repo/releases/latest")
    case "$url" in
        "https://github.com/$repo/releases/tag/"*) version=${url##*/} ;;
        *) echo "Cannot resolve latest release: $url" >&2; exit 1 ;;
    esac
fi
if ! printf '%s\n' "$version" | LC_ALL=C grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
    echo "Invalid stable version: $version" >&2
    exit 1
fi
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
trap 'exit 1' HUP INT TERM
archive="cmdgate_${version}_${os}_${arch}.tar.gz"
base="https://github.com/$repo/releases/download/$version"
# 交互终端显示进度条，重定向到日志时只保留阶段提示和错误
progress=--silent
if [ -t 2 ]; then
    progress=--progress-bar
fi
for asset in "$archive" SHA256SUMS; do
    printf 'Downloading %s...\n' "$asset" >&2
    curl --fail "$progress" --show-error --location --proto '=https' --proto-redir '=https' \
        --output "$work/$asset" "$base/$asset"
done
# 只校验当前平台的文件，拒绝缺失、重复或格式不正确的校验记录
printf 'Verifying SHA256 checksum...\n' >&2
expected=$(awk -v name="$archive" '$2 == name {print $1}' "$work/SHA256SUMS")
if [ "${#expected}" -ne 64 ] || ! printf '%s\n' "$expected" | LC_ALL=C grep -Eq '^[0-9a-fA-F]+$'; then
    echo "Missing or invalid checksum for $archive" >&2
    exit 1
fi
if [ "$checksum" = sha256sum ]; then
    actual=$(sha256sum "$work/$archive" | awk '{print $1}')
else
    actual=$(shasum -a 256 "$work/$archive" | awk '{print $1}')
fi
if [ "$actual" != "$expected" ]; then
    echo 'Checksum mismatch; nothing was installed.' >&2
    exit 1
fi
# 只解压所需二进制，不把归档的其他路径写入文件系统
printf 'Checksum verified. Extracting cmdgate %s...\n' "$version" >&2
tar -xzf "$work/$archive" -C "$work" cmdgate
if [ ! -f "$work/cmdgate" ] || [ -L "$work/cmdgate" ]; then
    echo 'Release archive does not contain a regular cmdgate binary.' >&2
    exit 1
fi
chmod 755 "$work/cmdgate"
if [ -n "$client_dir" ]; then
    case "$client_dir" in
        /*) ;;
        *) client_dir="$PWD/$client_dir" ;;
    esac
    if [ -d "$client_dir/cmdgate" ]; then
        echo 'Client destination is a directory; nothing was installed.' >&2
        exit 1
    fi
    printf 'Installing client to %s/cmdgate...\n' "$client_dir" >&2
    mkdir -p "$client_dir"
    target=$(mktemp "$client_dir/.cmdgate.XXXXXX")
    if ! cp "$work/cmdgate" "$target" || ! chmod 755 "$target" || ! mv -f "$target" "$client_dir/cmdgate"; then
        rm -f "$target"
        exit 1
    fi
    echo "Client installed: $client_dir/cmdgate (add $client_dir to PATH)"
elif [ "$(id -u)" -eq 0 ]; then
    printf 'Installing cmdgate %s and starting the system service...\n' "$version" >&2
    "$work/cmdgate" install --mode local
else
    printf 'Download and verification complete. Installing the system service requires sudo.\n' >&2
    printf 'Enter your login password if prompted; password input will not be displayed.\n' >&2
    sudo "$work/cmdgate" install --mode local
fi
