package winapi

import (
	"fmt"
	"unsafe"
)

var (
	procGetCursorPos     = user32.NewProc("GetCursorPos")
	procMonitorFromPoint = user32.NewProc("MonitorFromPoint")
	procGetMonitorInfoW  = user32.NewProc("GetMonitorInfoW")
)

// MONITOR_DEFAULTTONEAREST:点不在任何显示器上时返回最近的一个,
// 保证总能拿到一个可用的工作区。
const monitorDefaultToNearest = 2

// RECT 是 Win32 矩形。
type RECT struct {
	Left, Top, Right, Bottom int32
}

// Width 返回宽度。
func (r RECT) Width() int32 { return r.Right - r.Left }

// Height 返回高度。
func (r RECT) Height() int32 { return r.Bottom - r.Top }

type point struct{ X, Y int32 }

type monitorInfo struct {
	cbSize    uint32
	rcMonitor RECT
	rcWork    RECT
	dwFlags   uint32
}

// WorkAreaAtCursor 返回鼠标所在显示器的工作区(已排除任务栏)。
//
// 用鼠标位置而非主显示器:多屏时用户按热键的那一刻看的就是光标所在的屏幕,
// 面板应该出现在那里。
func WorkAreaAtCursor() (RECT, error) {
	var pt point
	if ret, _, callErr := procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt))); ret == 0 {
		return RECT{}, fmt.Errorf("GetCursorPos 失败: %w", callErr)
	}

	// POINT 按值传入:两个 int32 打包进一个 uintptr(x 低 32 位,y 高 32 位)
	packed := uintptr(uint32(pt.X)) | uintptr(uint32(pt.Y))<<32
	hMon, _, callErr := procMonitorFromPoint.Call(packed, monitorDefaultToNearest)
	if hMon == 0 {
		return RECT{}, fmt.Errorf("MonitorFromPoint 失败: %w", callErr)
	}

	mi := monitorInfo{cbSize: uint32(unsafe.Sizeof(monitorInfo{}))}
	if ret, _, callErr := procGetMonitorInfoW.Call(hMon, uintptr(unsafe.Pointer(&mi))); ret == 0 {
		return RECT{}, fmt.Errorf("GetMonitorInfoW 失败: %w", callErr)
	}
	return mi.rcWork, nil
}
