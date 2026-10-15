#!/usr/bin/env bash
# PasteWin 安装脚本(per-user,无需管理员权限)
#
#   ./install.sh                 安装并启动,同时开启开机自启
#   ./install.sh --no-autostart  不写自启项
#   ./install.sh --no-launch     装完不启动
#
# 安装位置:%LOCALAPPDATA%\Programs\PasteWin\  (程序)
#           %LOCALAPPDATA%\PasteWin\           (数据,卸载时默认保留)
set -euo pipefail

cd "$(dirname "$0")"

APP_NAME="PasteWin"
SRC="build/bin/${APP_NAME}.exe"
INSTALL_DIR_WIN="${LOCALAPPDATA}\\Programs\\${APP_NAME}"
INSTALL_EXE_WIN="${INSTALL_DIR_WIN}\\${APP_NAME}.exe"
STARTMENU_WIN="${APPDATA}\\Microsoft\\Windows\\Start Menu\\Programs\\${APP_NAME}.lnk"

AUTOSTART=1
LAUNCH=1
for arg in "$@"; do
    case "$arg" in
        --no-autostart) AUTOSTART=0 ;;
        --no-launch)    LAUNCH=0 ;;
        *) echo "未知参数: $arg" >&2; exit 2 ;;
    esac
done

if [[ ! -f "$SRC" ]]; then
    echo "找不到 $SRC,请先运行 ./build.sh" >&2
    exit 1
fi

echo "==> 停止正在运行的实例"
taskkill //F //IM "${APP_NAME}.exe" //T >/dev/null 2>&1 || true
sleep 1

echo "==> 安装到 ${INSTALL_DIR_WIN}"
mkdir -p "$(cygpath -u "$INSTALL_DIR_WIN")"
# 先删旧的再拷:直接覆盖会因目标正在运行而失败(已被上面的 taskkill 处理,这里是兜底)
rm -f "$(cygpath -u "$INSTALL_EXE_WIN")" 2>/dev/null || true
cp "$SRC" "$(cygpath -u "$INSTALL_EXE_WIN")"
echo "    $(ls -la "$(cygpath -u "$INSTALL_EXE_WIN")" | awk '{print $5}') 字节"

echo "==> 创建开始菜单快捷方式"
powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "
\$ws = New-Object -ComObject WScript.Shell
\$sc = \$ws.CreateShortcut('${STARTMENU_WIN}')
\$sc.TargetPath = '${INSTALL_EXE_WIN}'
\$sc.WorkingDirectory = '${INSTALL_DIR_WIN}'
\$sc.Description = 'PasteWin — 剪贴板历史管理'
\$sc.Save()
" 2>&1 | tail -2

if [[ "$AUTOSTART" == "1" ]]; then
    echo "==> 开启开机自启"
    # 与程序内 autostart.Enable() 写入的是同一个键值:
    # HKCU\Software\Microsoft\Windows\CurrentVersion\Run
    # 路径含空格,必须带引号,否则 Windows 会把它拆成多个参数
    powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "
Set-ItemProperty -Path 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run' \
    -Name '${APP_NAME}' -Value '\"${INSTALL_EXE_WIN}\"'
" 2>&1 | tail -2
fi

if [[ "$LAUNCH" == "1" ]]; then
    echo "==> 启动"
    "$(cygpath -u "$INSTALL_EXE_WIN")" >/dev/null 2>&1 &
    sleep 7
    if tasklist //FI "IMAGENAME eq ${APP_NAME}.exe" 2>/dev/null | grep -q "${APP_NAME}"; then
        echo "    运行中 ✓"
    else
        echo "    启动失败,请手动运行 ${INSTALL_EXE_WIN}" >&2
        exit 1
    fi
fi

cat <<EOF

安装完成。

  程序    ${INSTALL_EXE_WIN}
  数据    ${LOCALAPPDATA}\\${APP_NAME}\\
  快捷方式 开始菜单 → ${APP_NAME}
  开机自启 $([[ "$AUTOSTART" == "1" ]] && echo "已开启(可在应用内设置里关闭)" || echo "未开启")

程序常驻通知区域,不显示主窗口。按 Ctrl+Shift+V 唤出剪贴板面板。
右键托盘图标可打开设置、暂停捕获或退出。

卸载:./uninstall.sh
EOF
