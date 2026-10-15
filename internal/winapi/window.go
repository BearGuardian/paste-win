package winapi

import (
	"fmt"
	"log"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procGetForegroundWindow      = user32.NewProc("GetForegroundWindow")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procBringWindowToTop         = user32.NewProc("BringWindowToTop")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procAttachThreadInput        = user32.NewProc("AttachThreadInput")
	procIsIconic                 = user32.NewProc("IsIconic")
	procIsWindow                 = user32.NewProc("IsWindow")
	procShowWindow               = user32.NewProc("ShowWindow")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
	procGetWindowTextLengthW     = user32.NewProc("GetWindowTextLengthW")
	procGetCurrentThreadId       = kernel32.NewProc("GetCurrentThreadId")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procGetWindowRect            = user32.NewProc("GetWindowRect")
	procSetWindowPos             = user32.NewProc("SetWindowPos")
)

// SWP_NOZORDER / SWP_NOACTIVATE:只改几何,不动 Z 序、不抢焦点。
// 面板是靠热键唤出的,改尺寸时抢焦点会把用户正在输入的目标窗口顶掉。
const (
	swpNoZOrder   = 0x0004
	swpNoActivate = 0x0010
)

// SetWindowBounds 把窗口**外框**放到屏幕绝对坐标的 (x, y, w, h)。
//
// 必须绕过 Wails 的 runtime.WindowSetSize / WindowSetPosition ——
// 这两个方法在 Windows 后端上都不能用来做多屏绝对定位,见 DESIGN.md 8.16:
//
//   - WindowSetSize 会把尺寸再乘一遍"窗口当前所在显示器"的 DPI 缩放,
//     在 125% 的屏幕上 1920 会变成 2400;
//   - WindowSetPosition 把 (x, y) 当成相对**窗口当前所在显示器工作区**
//     左上角的偏移,而不是屏幕绝对坐标。
//
// 本进程声明了 permonitorv2,SetWindowPos 收发的都是物理像素,不做任何换算。
func SetWindowBounds(hwnd uintptr, x, y, w, h int) error {
	if hwnd == 0 || !IsWindow(hwnd) {
		return fmt.Errorf("窗口句柄无效: %d", hwnd)
	}
	if w <= 0 || h <= 0 {
		return fmt.Errorf("窗口尺寸非法: %d×%d", w, h)
	}
	ret, _, callErr := procSetWindowPos.Call(hwnd, 0,
		uintptr(int32(x)), uintptr(int32(y)), uintptr(int32(w)), uintptr(int32(h)),
		swpNoZOrder|swpNoActivate)
	if ret == 0 {
		return fmt.Errorf("SetWindowPos 失败: %w", callErr)
	}
	return nil
}

// WindowRect 返回窗口在屏幕坐标系里的矩形(物理像素)。
//
// 用来回读"我设的尺寸/位置到底生效了没有" —— 不回读就无法区分
// "算错了"和"还没生效"。
func WindowRect(hwnd uintptr) (RECT, error) {
	if hwnd == 0 || !IsWindow(hwnd) {
		return RECT{}, fmt.Errorf("窗口句柄无效: %d", hwnd)
	}
	var r RECT
	if ret, _, callErr := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r))); ret == 0 {
		return RECT{}, fmt.Errorf("GetWindowRect 失败: %w", callErr)
	}
	return r, nil
}

// GetForegroundWindow 返回当前前台窗口句柄。
func GetForegroundWindow() uintptr {
	ret, _, _ := procGetForegroundWindow.Call()
	return ret
}

// GetCurrentThreadID 返回调用线程的 ID。
func GetCurrentThreadID() uint32 {
	ret, _, _ := procGetCurrentThreadId.Call()
	return uint32(ret)
}

// WindowThreadProcessID 返回窗口所属的线程 ID 与进程 ID。
func WindowThreadProcessID(hwnd uintptr) (threadID, processID uint32) {
	var pid uint32
	ret, _, _ := procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	return uint32(ret), pid
}

// IsWindow 判断句柄是否为有效窗口。窗口可能在面板弹出期间被关掉,
// 回贴前必须校验,否则按键会发给不存在的窗口。
func IsWindow(hwnd uintptr) bool {
	ret, _, _ := procIsWindow.Call(hwnd)
	return ret != 0
}

// IsWindowVisible 判断窗口当前是否可见。
//
// 用于等窗口真正显示出来再抢前台:对尚未显示的窗口调 SetForegroundWindow
// 一定失败,而窗口显示是异步的(见 App.focusPanel)。
func IsWindowVisible(hwnd uintptr) bool {
	ret, _, _ := procIsWindowVisible.Call(hwnd)
	return ret != 0
}

// WindowTitle 返回窗口标题,失败返回空串。
func WindowTitle(hwnd uintptr) string {
	n, _, _ := procGetWindowTextLengthW.Call(hwnd)
	if n == 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), n+1)
	return windows.UTF16ToString(buf)
}

var procFindWindowW = user32.NewProc("FindWindowW")
var procGetClassNameW = user32.NewProc("GetClassNameW")

// ClassName 返回窗口类名,取不到返回空串。
//
// 排查焦点问题时很关键:类名能说明目标是不是"自绘界面"。
// CEF/Electron 一类的程序(Chrome_WidgetWin_* 等)整块界面是画出来的,
// 输入框并不是真正的子窗口,跨进程 SetFocus 对它毫无意义 ——
// 它的焦点由程序自己维护,只能靠"激活顶层窗口 + 等它自己恢复焦点"。
func ClassName(hwnd uintptr) string {
	if hwnd == 0 {
		return ""
	}
	buf := make([]uint16, 256)
	n, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

// FindWindowByTitle 按标题查找顶层窗口,找不到返回 0。
//
// 用于拿到 PasteWin 自己的主窗口句柄:Wails 不对外暴露 HWND,
// 而唤起面板时需要主动抢焦点(见 app.ShowPanel)。
func FindWindowByTitle(title string) uintptr {
	titlePtr, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return 0
	}
	hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(titlePtr)))
	return hwnd
}

// ForceForeground 把 hwnd 强行提到前台。
//
// 分两级尝试,从最轻的做起:
//
//  1. 直接 SetForegroundWindow。本进程持有全局热键,Windows 通常会放行;
//     面板把焦点交还给目标窗口时我们自己就是前台进程,也走这条。
//  2. 挂到"当前前台线程"的输入队列上再试。这是解除前台锁定的标准变通:
//     共享输入状态后本线程即被视为前台。
//     **只挂对方,不能连自己的窗口线程一起挂** —— 把本进程的消息线程
//     也拉进输入队列共享,SetForegroundWindow 反而会失败。这个坑实测踩过。
//
// foregroundSettle 是等待前台切换落定的时长。
//
// 前台切换是**异步**的:SetForegroundWindow 返回时,系统只是把激活请求
// 交给了目标线程,真正切换完成还要等它处理。这中间 GetForegroundWindow()
// 会读到 0(原前台已取消激活、新前台尚未就位)。
// 读一次就判定失败会让回贴凭空放弃 —— 实测就是这么丢的。
const foregroundSettle = 150 * time.Millisecond

// waitForeground 轮询等待 hwnd 真正成为前台窗口。
func waitForeground(hwnd uintptr) bool {
	deadline := time.Now().Add(foregroundSettle)
	for {
		if GetForegroundWindow() == hwnd {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func ForceForeground(hwnd uintptr) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if hwnd == 0 || !IsWindow(hwnd) {
		return fmt.Errorf("目标窗口无效: %d", hwnd)
	}
	curThread := GetCurrentThreadID()
	fg := GetForegroundWindow()
	if fg == hwnd {
		return nil
	}

	fgThread, _ := WindowThreadProcessID(fg)
	targetThread, _ := WindowThreadProcessID(hwnd)
	log.Printf("抢前台: 目标=%d(%s) 线程=%d | 当前前台=%d(%s) 线程=%d | 本线程=%d",
		hwnd, ClassName(hwnd), targetThread,
		fg, ClassName(fg), fgThread, curThread)

	// 最小化的窗口直接 SetForegroundWindow 无效,先还原
	if iconic, _, _ := procIsIconic.Call(hwnd); iconic != 0 {
		procShowWindow.Call(hwnd, SW_RESTORE)
	}

	// 1. 直接试
	procBringWindowToTop.Call(hwnd)
	ret, _, err := procSetForegroundWindow.Call(hwnd)
	log.Printf("抢前台: 直接 SetForegroundWindow ret=%d err=%v → 前台=%d",
		ret, err, GetForegroundWindow())
	if waitForeground(hwnd) {
		return nil
	}

	// 2. 挂到当前前台线程再试
	if fgThread != 0 && fgThread != curThread {
		ar, _, aerr := procAttachThreadInput.Call(uintptr(curThread), uintptr(fgThread), 1)
		log.Printf("抢前台: AttachThreadInput(本线程=%d, 前台线程=%d) ret=%d err=%v",
			curThread, fgThread, ar, aerr)
		if ar != 0 {
			procSetForegroundWindow.Call(hwnd)
			procAttachThreadInput.Call(uintptr(curThread), uintptr(fgThread), 0)
			log.Printf("抢前台: 挂载后 SetForegroundWindow → 前台=%d", GetForegroundWindow())
			if waitForeground(hwnd) {
				return nil
			}
		}
	}

	// 两级都没成功就放弃。
	//
	// 曾经这里还有第三级"模拟 Alt 轻敲"来强行解除前台锁定,后来撤掉了:
	// 它会往当前焦点窗口发一次 Alt,可能激活对方的菜单栏;
	// 而实测中生效的一直是前两级,那一级没有验证过价值,留着只是风险。
	return fmt.Errorf("SetForegroundWindow 未生效(目标窗口可能拒绝激活)")
}
