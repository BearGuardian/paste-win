package winapi

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	shcore = windows.NewLazySystemDLL("shcore.dll")

	procGetCursorPos      = user32.NewProc("GetCursorPos")
	procMonitorFromPoint  = user32.NewProc("MonitorFromPoint")
	procMonitorFromWindow = user32.NewProc("MonitorFromWindow")
	procGetMonitorInfoW   = user32.NewProc("GetMonitorInfoW")
	procGetDpiForMonitor  = shcore.NewProc("GetDpiForMonitor")
)

// MONITOR_DEFAULTTONEAREST:点不在任何显示器上时返回最近的一个,
// 保证总能拿到一个可用的工作区。
const monitorDefaultToNearest = 2

// MDT_EFFECTIVE_DPI:按显示器实际生效的缩放取 DPI(用户改缩放比例后即为新值)。
const mdtEffectiveDPI = 0

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

// Monitor 是鼠标所在显示器的几何与缩放。
//
// 坐标一律是**物理像素**。本程序在清单里声明了 permonitorv2
// (见 build/windows/wails.exe.manifest),所以 Win32 不会替我们做
// 任何 DPI 虚拟化 —— 读到什么就是屏幕上的真实像素。
type Monitor struct {
	Work RECT   // 工作区(已排除任务栏),物理像素
	DPI  uint32 // 有效 DPI,96 即 100%
}

// Scale 返回缩放倍数,1.0 表示 100%。取不到 DPI 时按 100% 处理。
func (m Monitor) Scale() float64 {
	if m.DPI == 0 {
		return 1
	}
	return float64(m.DPI) / 96
}

// CursorPos 返回鼠标在屏幕坐标系里的位置(物理像素)。
func CursorPos() (x, y int32, err error) {
	var pt point
	if ret, _, callErr := procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt))); ret == 0 {
		return 0, 0, fmt.Errorf("GetCursorPos 失败: %w", callErr)
	}
	return pt.X, pt.Y, nil
}

// MonitorAtCursor 返回鼠标所在显示器的工作区与缩放。
//
// 用鼠标位置而非主显示器:多屏时用户按热键的那一刻看的就是光标所在的屏幕,
// 面板应该出现在那里。
func MonitorAtCursor() (Monitor, error) {
	pt := point{}
	if ret, _, callErr := procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt))); ret == 0 {
		return Monitor{}, fmt.Errorf("GetCursorPos 失败: %w", callErr)
	}

	// POINT 按值传入:两个 int32 打包进一个 uintptr(x 低 32 位,y 高 32 位)
	packed := uintptr(uint32(pt.X)) | uintptr(uint32(pt.Y))<<32
	hMon, _, callErr := procMonitorFromPoint.Call(packed, monitorDefaultToNearest)
	if hMon == 0 {
		return Monitor{}, fmt.Errorf("MonitorFromPoint 失败: %w", callErr)
	}

	mi := monitorInfo{cbSize: uint32(unsafe.Sizeof(monitorInfo{}))}
	if ret, _, callErr := procGetMonitorInfoW.Call(hMon, uintptr(unsafe.Pointer(&mi))); ret == 0 {
		return Monitor{}, fmt.Errorf("GetMonitorInfoW 失败: %w", callErr)
	}
	return describe(hMon, mi.rcWork), nil
}

// MonitorOfWindow 返回窗口所在显示器的工作区与缩放。
//
// 用于"回读窗口现状"的场景(比如收起面板时把实际高度换算回逻辑像素):
// 这时该看的是窗口在哪块屏上,而不是光标在哪 —— 用户完全可能把光标
// 挪到别的显示器上再按热键收起面板。
func MonitorOfWindow(hwnd uintptr) (Monitor, error) {
	hMon, _, callErr := procMonitorFromWindow.Call(hwnd, monitorDefaultToNearest)
	if hMon == 0 {
		return Monitor{}, fmt.Errorf("MonitorFromWindow 失败: %w", callErr)
	}

	mi := monitorInfo{cbSize: uint32(unsafe.Sizeof(monitorInfo{}))}
	if ret, _, callErr := procGetMonitorInfoW.Call(hMon, uintptr(unsafe.Pointer(&mi))); ret == 0 {
		return Monitor{}, fmt.Errorf("GetMonitorInfoW 失败: %w", callErr)
	}
	return describe(hMon, mi.rcWork), nil
}

// describe 组装 Monitor,并补上显示器的有效 DPI。
func describe(hMon uintptr, work RECT) Monitor {
	m := Monitor{Work: work}

	// DPI 取不到不算致命:按 100% 处理,面板高度会略小于预期,但位置仍正确。
	var dpiX, dpiY uint32
	if ret, _, _ := procGetDpiForMonitor.Call(hMon, mdtEffectiveDPI,
		uintptr(unsafe.Pointer(&dpiX)), uintptr(unsafe.Pointer(&dpiY))); ret == 0 {
		m.DPI = dpiX
	}
	return m
}
