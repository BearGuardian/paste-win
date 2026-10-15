#!/usr/bin/env bash
# PasteWin 卸载脚本
#
#   ./uninstall.sh              卸载程序,保留历史数据
#   ./uninstall.sh --purge      连数据一起删除
set -euo pipefail

APP_NAME="PasteWin"
INSTALL_DIR_WIN="${LOCALAPPDATA}\\Programs\\${APP_NAME}"
STARTMENU_WIN="${APPDATA}\\Microsoft\\Windows\\Start Menu\\Programs\\${APP_NAME}.lnk"
DATA_DIR_WIN="${LOCALAPPDATA}\\${APP_NAME}"

PURGE=0
for arg in "$@"; do
    case "$arg" in
        --purge) PURGE=1 ;;
        *) echo "未知参数: $arg" >&2; exit 2 ;;
    esac
done

echo "==> 停止正在运行的实例"
# 先请求优雅退出(会走 Shutdown 清理、注销热键、移除托盘图标),
# 3 秒内没退干净再强杀
INSTALL_EXE_WIN="${INSTALL_DIR_WIN}\\${APP_NAME}.exe"
if [[ -f "$(cygpath -u "$INSTALL_EXE_WIN")" ]]; then
    "$(cygpath -u "$INSTALL_EXE_WIN")" --quit >/dev/null 2>&1 || true
    sleep 3
fi
if tasklist //FI "IMAGENAME eq ${APP_NAME}.exe" 2>/dev/null | grep -q "${APP_NAME}"; then
    echo "    优雅退出超时,强制结束"
    taskkill //F //IM "${APP_NAME}.exe" //T >/dev/null 2>&1 || true
    sleep 1
fi

echo "==> 移除开机自启"
powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "
Remove-ItemProperty -Path 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run' \
    -Name '${APP_NAME}' -ErrorAction SilentlyContinue
" 2>&1 | tail -2

echo "==> 删除开始菜单快捷方式"
rm -f "$(cygpath -u "$STARTMENU_WIN")" 2>/dev/null || true

echo "==> 删除程序目录"
rm -rf "$(cygpath -u "$INSTALL_DIR_WIN")" 2>/dev/null || true

if [[ "$PURGE" == "1" ]]; then
    echo "==> 删除历史数据 ${DATA_DIR_WIN}"
    rm -rf "$(cygpath -u "$DATA_DIR_WIN")" 2>/dev/null || true
    echo "    已删除"
else
    echo "==> 保留历史数据 ${DATA_DIR_WIN}"
    echo "    如需一并删除:./uninstall.sh --purge"
fi

echo
echo "卸载完成。"
