#!/bin/bash
# 编译 GitPuller。默认编译当前平台；传 cross 编译 mac/linux/windows 三平台产物。
set -e
cd "$(dirname "$0")"

GO=$(command -v go || true)
if [ -z "$GO" ]; then
  for c in /opt/homebrew/bin/go /usr/local/go/bin/go; do
    [ -x "$c" ] && GO="$c" && break
  done
fi
if [ -z "$GO" ]; then
  echo "未找到 go，请先安装 Go 1.22+" >&2
  exit 1
fi

CGO_ENABLED=0 "$GO" build -trimpath -ldflags "-s -w" -o gitpuller .
echo "已编译: $(pwd)/gitpuller"

if [ "$1" = "cross" ]; then
  out=dist
  mkdir -p "$out"
  for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64; do
    os=${t%/*}; arch=${t#*/}
    bin=gitpuller
    [ "$os" = "windows" ] && bin=gitpuller.exe
    echo "→ $os/$arch"
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch "$GO" build -trimpath -ldflags "-s -w" -o "$out/${os}-${arch}-$bin" .
  done
  echo "跨平台产物已输出到 $(pwd)/$out"
fi
