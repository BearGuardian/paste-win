// Command deskprobe 打印当前进程所在的窗口站与桌面,以及输入相关 API 的可用性。
//
// 用来判断"注入的按键为什么没反应":进程一旦不在**交互式输入桌面**上
// (被沙箱、服务会话或非交互窗口站启动),GetCursorPos、SetForegroundWindow、
// SendInput 会失败或静默无效。这类失败从应用日志里只看得到一句
// "Access is denied",很难反推到根因,所以单独做个探针。
//
// 用法:
//
//	deskprobe
//
// 正常在用户桌面上运行的输出应当是:桌面名 Default,OpenInputDesktop 成功。
package main

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procGetCursorPos              = user32.NewProc("GetCursorPos")
	procGetForegroundWindow       = user32.NewProc("GetForegroundWindow")
	procOpenInputDesktop          = user32.NewProc("OpenInputDesktop")
	procGetThreadDesktop          = user32.NewProc("GetThreadDesktop")
	procGetProcessWindowStation   = user32.NewProc("GetProcessWindowStation")
	procGetUserObjectInformationW = user32.NewProc("GetUserObjectInformationW")
	procGetCurrentThreadId        = kernel32.NewProc("GetCurrentThreadId")
)

type point struct{ X, Y int32 }

func main() {
	// 桌面与窗口站是**线程**属性,必须锁线程才能拿到有意义的答案
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	var p point
	ret, _, err := procGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	fmt.Printf("GetCursorPos         ret=%d err=%v pos=(%d,%d)\n", ret, err, p.X, p.Y)

	fg, _, _ := procGetForegroundWindow.Call()
	fmt.Printf("GetForegroundWindow  hwnd=%d\n", fg)

	station, _, _ := procGetProcessWindowStation.Call()
	fmt.Printf("窗口站               name=%q\n", objectName(station))

	tid, _, _ := procGetCurrentThreadId.Call()
	desktop, _, err := procGetThreadDesktop.Call(tid)
	fmt.Printf("当前线程桌面         err=%v name=%q\n", err, objectName(desktop))

	input, _, err := procOpenInputDesktop.Call(0, 0, 0x0100 /* DESKTOP_READOBJECTS */)
	fmt.Printf("OpenInputDesktop     err=%v name=%q\n", err, objectName(input))

	if input == 0 {
		fmt.Println("\n结论:拿不到输入桌面 —— 按键注入与抢前台都不会生效")
	} else {
		fmt.Println("\n结论:可以访问输入桌面")
	}
}

// objectName 读取 USER 对象的名称(窗口站或桌面)。
func objectName(h uintptr) string {
	if h == 0 {
		return ""
	}
	buf := make([]uint16, 256)
	var need uint32
	ret, _, _ := procGetUserObjectInformationW.Call(
		h, 2, // UOI_NAME
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)*2),
		uintptr(unsafe.Pointer(&need)),
	)
	if ret == 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}
