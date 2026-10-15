#!/usr/bin/env bash
# PasteWin 构建脚本(Git Bash)
#
# 封装了本机环境的几个坑,详见 DESIGN.md「构建」一节:
#   1. Go 不在 PATH 里,在 .tools/go
#   2. proxy.golang.org 不通,必须走 goproxy.cn
#   3. CGO_ENABLED=0,SQLite 用纯 Go 驱动
#   4. wails build 的绑定生成步骤在本项目上极慢(见下),默认跳过
set -euo pipefail

cd "$(dirname "$0")"

export PATH="/d/code-env/.tools/go/bin:/c/Users/suyz2/go/bin:$PATH"
export GOPROXY=https://goproxy.cn,direct
export GOSUMDB=off
export CGO_ENABLED=0

# -skipbindings 的原因:
# Wails 生成 JS 绑定时会用 go/packages 加载整个依赖图,而 modernc.org/sqlite
# 是 C 转译产物、包数极多,这一步在本机要跑 11 分钟以上。
# 本项目的前端直接调用运行时注入的 window.go.main.App.Xxx(),
# 不依赖生成的 wailsjs 文件,因此可以安全跳过,构建回到 4 秒。
# 只有当 Bind 的方法签名变了、且需要 IDE 类型提示时,才偶尔跑一次完整构建:
#   ./build.sh --full
BUILD_FLAGS=(-skipbindings -nosyncgomod)
if [[ "${1:-}" == "--full" ]]; then
    echo "完整构建(含绑定生成,可能耗时十余分钟)…"
    BUILD_FLAGS=()
fi

echo "==> 运行测试"
go test ./internal/... 2>&1 | grep -v "no test files" || true

echo "==> 静态检查"
go vet ./...

echo "==> 构建应用"
wails build "${BUILD_FLAGS[@]}"

echo "==> 构建开发工具"
mkdir -p build
go build -o build/sendkeys.exe  ./tools/sendkeys
go build -o build/keytarget.exe ./tools/keytarget
go build -o build/focustest.exe ./tools/focustest
go build -o build/hotkeyprobe.exe ./tools/hotkeyprobe
go build -o build/deskprobe.exe ./tools/deskprobe

# 无控制台版本。控制台程序一运行就会把焦点抢到终端,触发面板的
# "失去焦点自动收起",回贴测试根本走不到注入那一步。
go build -ldflags "-H=windowsgui" -o build/sendkeysw.exe  ./tools/sendkeys
go build -ldflags "-H=windowsgui" -o build/keytargetw.exe ./tools/keytarget

echo
echo "完成: build/bin/PasteWin.exe"
ls -la build/bin/PasteWin.exe
