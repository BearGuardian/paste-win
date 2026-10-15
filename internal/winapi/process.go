package winapi

import (
	"fmt"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procOpenProcess                = kernel32.NewProc("OpenProcess")
	procCloseHandle                = kernel32.NewProc("CloseHandle")
	procQueryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")
)

// ProcessImagePath 返回进程可执行文件的完整路径。
//
// 用 PROCESS_QUERY_LIMITED_INFORMATION 而非 PROCESS_QUERY_INFORMATION:
// 前者对更高完整性级别的进程(如以管理员身份运行的程序)也能打开,
// 后者会被拒绝。剪贴板来源经常是提权程序,这个区别很实际。
func ProcessImagePath(pid uint32) (string, error) {
	if pid == 0 {
		return "", fmt.Errorf("进程 ID 为 0")
	}
	h, _, callErr := procOpenProcess.Call(PROCESS_QUERY_LIMITED_INFORMATION, 0, uintptr(pid))
	if h == 0 {
		return "", fmt.Errorf("OpenProcess(%d) 失败: %w", pid, callErr)
	}
	defer procCloseHandle.Call(h)

	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	ret, _, callErr := procQueryFullProcessImageNameW.Call(
		h, 0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if ret == 0 {
		return "", fmt.Errorf("QueryFullProcessImageNameW 失败: %w", callErr)
	}
	return windows.UTF16ToString(buf[:size]), nil
}

// ProcessName 返回进程名(不含路径与扩展名),用作卡片上的来源标识。
func ProcessName(pid uint32) string {
	path, err := ProcessImagePath(pid)
	if err != nil {
		return ""
	}
	base := filepath.Base(path)
	return base[:len(base)-len(filepath.Ext(base))]
}

// ClipboardSource 描述剪贴板内容的来源。
type ClipboardSource struct {
	PID      uint32
	App      string // 进程名,如 "chrome"
	Title    string // 来源窗口标题
	OwnerHWND uintptr
}

// GetClipboardSource 推断剪贴板内容的来源应用。
//
// 走 GetClipboardOwner 拿所有者窗口 —— 注意并非所有程序都会设置 owner
// (例如某些终端直接调 SetClipboardData 而不带窗口),此时各字段为空,
// 调用方需按"来源未知"处理,不能当作错误。
func GetClipboardSource() ClipboardSource {
	owner := ClipboardOwner()
	if owner == 0 {
		return ClipboardSource{}
	}
	_, pid := WindowThreadProcessID(owner)
	return ClipboardSource{
		PID:       pid,
		App:       ProcessName(pid),
		Title:     WindowTitle(owner),
		OwnerHWND: owner,
	}
}
