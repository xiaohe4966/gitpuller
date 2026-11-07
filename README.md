# GitPuller

[![build](https://github.com/xiaohe4966/gitpuller/actions/workflows/build.yml/badge.svg)](https://github.com/xiaohe4966/gitpuller/actions/workflows/build.yml)
[![license](https://img.shields.io/badge/license-MIT-157a5b.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.22%2B-00ADD8.svg)](https://go.dev)

A tiny cross-platform tool that runs `git pull` on **multiple repositories on a schedule**, with a built-in web UI, per-repo toggles, live logs and one-click pause. Single Go binary, zero third-party dependencies — runs on macOS, Linux and Windows from the same source.

一个单文件 Go 程序：内置 Web 界面、定时调度、实时日志、开关控制，支持添加任意多个仓库目录。零第三方依赖（只用 Go 标准库），`CGO_ENABLED=0` 编译，macOS / Linux / Windows 同一份代码。

> 适合：多个仓库要随时保持最新（改完代码换机器、服务器常驻拉取、盯着一堆项目的分支），懒得每次都手动 `cd && git pull`。

## 特性

- 多目录统一管理，每个目录独立间隔（30 秒 ~ 30 分钟）与启停开关
- 全局一键 **启动 / 暂停**，随时可停，不动你的工作区
- 默认 `ff-only` 快进策略：**绝不产生合并提交、绝不留冲突**
- 有未提交改动时自动跳过本次拉取，不 stash、不 reset、不覆盖
- 实时日志流（SSE 推送），失败原因翻成人话
- 单二进制，配置存 `~/.gitpuller/config.json`，重开即恢复

## 安装

```bash
# 1) 直接用 Go 安装（需要 Go 1.22+）
go install github.com/xiaohe4966/gitpuller@latest

# 2) 或克隆后编译
git clone https://github.com/xiaohe4966/gitpuller.git
cd gitpuller
./build.sh            # 当前平台
./build.sh cross      # 顺便编译 mac / linux / windows 五套产物到 dist/
```

也可以从 [Releases](https://github.com/xiaohe4966/gitpuller/releases) 下载预编译版本（如已发布）。

## 为什么不是 Wails / Fyne

| 方案 | 体积/依赖 | 跨端 | 适合本需求吗 |
| --- | --- | --- | --- |
| **标准库 net/http + go:embed（本项目）** | 单文件约 8MB，零依赖，无 Node、无 cgo | 三平台直接交叉编译 | **最合适**。界面在浏览器里，不需要 webview、不需要签名、不需要装运行时 |
| Wails v2 | 需要 Node 构建前端 + 系统 webview，产物 10MB+ | 支持（用系统 WebView） | 想要「独立窗口 App 感」时选它；但前端多一层构建链，升级维护成本高 |
| Fyne | 需要 cgo / OpenGL 依赖 | 交叉编译麻烦 | 纯 Go 但桌面渲染质量一般，表单控件也不如网页灵活 |
| Gio | 无 cgo（多数平台），但 API 偏底层 | 可以 | 适合自绘 UI，做这种表单后台太重 |
| Lorca | 依赖本机 Chrome | 需要 Chrome | 不如直接开浏览器稳 |

结论：**这类「后台常驻 + 表单配置 + 日志流」的工具，用标准库加一个内嵌网页最省事**；只有当你需要托盘图标、独立窗口、无浏览器依赖时，才值得换 Wails。

## 快速开始

```bash
cd gitpuller
./build.sh            # 编译当前平台，产出 ./gitpuller
./gitpuller           # 默认监听 http://127.0.0.1:7788 并自动打开浏览器
```

命令行参数：

| 参数 | 说明 |
| --- | --- |
| `-addr 127.0.0.1:7788` | 监听地址，端口被占用会自动顺延 |
| `-config <file>` | 配置文件路径，默认 `~/.gitpuller/config.json` |
| `-open=false` | 启动时不自动打开浏览器 |
| `-once` | 只拉取一轮就退出，适合 cron / 定时任务 |
| `./build.sh cross` | 另外交叉编译 macOS(arm64/amd64)、Linux、Windows 产物到 `dist/` |

## 界面能做什么

- 顶栏：**启动 / 暂停**全局定时任务，**全部拉取**，**添加目录**
- 每个目录一张卡片：
  - 启用开关（单独暂停某个仓库）
  - 间隔下拉：30 秒 / 1 分钟 / 2 / 5 / 10 / 30 分钟
  - 策略下拉：`ff-only`（默认，最安全）/ `rebase` / `merge`
  - 立即拉取、日志（实时流）、删除（只从列表移除，不动仓库文件）
- 状态一目了然：待运行 / 拉取中 / 已是最新 / 已更新 +N / 失败

失败原因会被翻译成人话，例如「有未提交的本地改动，已跳过本次拉取」「本地与远程已分叉，ff-only 无法快进」「网络不可达」。

## 安全设计

- 默认 **ff-only**：只做快进，绝不会在你的仓库里生成合并提交或留下冲突。
- 工作区有未提交改动时 git 会拒绝 pull，程序只记录跳过，**不 stash、不 reset、不覆盖**。
- `GIT_TERMINAL_PROMPT=0`：绝不停在密码输入上；单次拉取 180 秒超时。
- 同一目录同时只允许一个拉取任务，避免 git 锁冲突。
- 配置原子写入，崩了也不会写坏 JSON。

## 开机自启 / 后台常驻

macOS（推荐）：

```bash
./install-launchd.sh          # 装成登录自启的 LaunchAgent
./install-launchd.sh uninstall # 卸载
```

Linux（systemd user unit）或任意系统用 cron：

```bash
# 每分钟拉一次，不需要常驻进程
* * * * * /path/to/gitpuller -once >> ~/.gitpuller/cron.log 2>&1
```

Windows：把 `gitpuller.exe` 快捷方式丢进「启动」文件夹，或注册计划任务。

## 文件结构

```
gitpuller/
├── main.go            # 命令行、HTTP 路由、SSE、系统目录选择框、打开浏览器
├── manager.go         # 项目模型、调度器、git 执行与结果归类
├── ui.html            # 内嵌单页界面（go:embed）
├── build.sh           # 编译 / 交叉编译
├── install-launchd.sh # macOS 自启
├── LICENSE            # MIT
└── ~/.gitpuller/config.json  # 运行时配置（自动生成）
```

## 想继续扩展的方向

- 拉取成功时发系统通知（macOS `osascript -e 'display notification ...'`）
- 只在有更新时通知，避免每分钟打扰
- 记录每次拉到的提交列表（`git log --oneline before..after`）
- 支持 `git fetch` + `git status` 的「只检测不合并」模式

## License

[MIT](LICENSE) © 2026 xiaohe4966
