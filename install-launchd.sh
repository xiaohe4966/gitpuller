#!/bin/bash
# 把 GitPuller 注册为 macOS 登录自启服务（LaunchAgent）。
# 用法: ./install-launchd.sh          安装并启动
#       ./install-launchd.sh uninstall 卸载
set -e
DIR="$(cd "$(dirname "$0")" && pwd)"
LABEL="com.local.gitpuller"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"

if [ "$1" = "uninstall" ]; then
  launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || launchctl unload "$PLIST" 2>/dev/null || true
  rm -f "$PLIST"
  echo "已卸载登录自启，服务已停止。"
  exit 0
fi

[ -x "$DIR/gitpuller" ] || { echo "先执行 ./build.sh 生成可执行文件" >&2; exit 1; }

mkdir -p "$HOME/Library/LaunchAgents" "$HOME/.gitpuller"
cat > "$PLIST" <<PLISTEOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>$DIR/gitpuller</string>
    <string>-open=false</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>$HOME/.gitpuller/server.log</string>
  <key>StandardErrorPath</string><string>$HOME/.gitpuller/server.err.log</string>
</dict>
</plist>
PLISTEOF

launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$PLIST"
echo "已安装: $PLIST（开机登录后自动在 http://127.0.0.1:7788 运行）"
