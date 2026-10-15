package winapi

import (
	"fmt"
	"runtime"
	"unsafe"
)

var (
	procGetGUIThreadInfo = user32.NewProc("GetGUIThreadInfo")
	procSetFocus         = user32.NewProc("SetFocus")
	procGetFocus         = user32.NewProc("GetFocus")
)

// GUITHREADINFO 的 x64 布局:
// cbSize(4) + flags(4) + 六个 HWND(各 8) + RECT(16) = 72 字节。
type guiThreadInfo struct {
	cbSize        uint32
	flags         uint32
	hwndActive    uintptr
	hwndFocus     uintptr
	hwndCapture   uintptr
	hwndMenuOwner uintptr
	hwndMoveSize  uintptr
	hwndCaret     uintptr
	rcCaret       RECT
}

// FocusedWindow 返回指定线程当前拥有键盘焦点的窗口,取不到返回 0。
//
// threadID 传 0 表示当前前台线程。
func FocusedWindow(threadID uint32) uintptr {
	info := guiThreadInfo{cbSize: uint32(unsafe.Sizeof(guiThreadInfo{}))}
	ret, _, _ := procGetGUIThreadInfo.Call(
		uintptr(threadID), uintptr(unsafe.Pointer(&info)),
	)
	if ret == 0 {
		return 0
	}
	return info.hwndFocus
}

// SetFocusCrossThread 把键盘焦点设到 hwnd 上,即使它属于别的线程。
//
// SetFocus 只能作用于调用线程输入队列里的窗口,跨线程直接调用会失败。
// 先 AttachThreadInput 把两个输入队列临时接在一起,焦点就能跨线程设置了。
//
// 和 ForceForeground 一样必须锁 OS 线程:attach/detach 以"当前线程"为一方,
// goroutine 中途换线程会导致 detach 作用在错误的线程上,输入队列永久粘连。
func SetFocusCrossThread(targetThread uint32, hwnd uintptr) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if hwnd == 0 || !IsWindow(hwnd) {
		return fmt.Errorf("目标控件无效: %d", hwnd)
	}

	curThread := GetCurrentThreadID()
	if targetThread == 0 || targetThread == curThread {
		procSetFocus.Call(hwnd)
		return nil
	}

	ret, _, callErr := procAttachThreadInput.Call(uintptr(curThread), uintptr(targetThread), 1)
	if ret == 0 {
		return fmt.Errorf("AttachThreadInput 失败: %w", callErr)
	}
	defer procAttachThreadInput.Call(uintptr(curThread), uintptr(targetThread), 0)

	procSetFocus.Call(hwnd)
	return nil
}

// CurrentFocus 返回调用线程当前拥有焦点的窗口。
func CurrentFocus() uintptr {
	ret, _, _ := procGetFocus.Call()
	return ret
}

// FocusTarget 描述唤起面板前,用户正在操作的目标。
//
// 光记窗口不够:用户在窗口内部某个输入框里打字时,激活被面板抢走后
// 焦点不会自动回到那个输入框,回贴的 Ctrl+V 会落到顶层窗口上而静默失效。
// 因此要把"哪个控件持有焦点"一并记下来。
type FocusTarget struct {
	Window uintptr // 前台顶层窗口
	Thread uint32  // 该窗口所属线程
	Focus  uintptr // 该线程内持有焦点的控件,可能为 0
}

// CaptureFocusTarget 记录当前前台窗口及其内部的焦点控件。
//
// 必须在面板抢走激活之前调用,否则记到的是 PasteWin 自己。
func CaptureFocusTarget() FocusTarget {
	hwnd := GetForegroundWindow()
	if hwnd == 0 {
		return FocusTarget{}
	}
	threadID, _ := WindowThreadProcessID(hwnd)
	return FocusTarget{
		Window: hwnd,
		Thread: threadID,
		Focus:  FocusedWindow(threadID),
	}
}

// Restore 把焦点还给当初记录的目标窗口与控件。
func (t FocusTarget) Restore() error {
	if t.Window == 0 {
		return fmt.Errorf("没有可还原的目标窗口")
	}
	if err := ForceForeground(t.Window); err != nil {
		return err
	}
	// 控件焦点没记到(例如用户当时操作的是没有子控件的窗口)就不动它,
	// 此时顶层窗口拿焦点即可,粘贴仍然有效。
	if t.Focus != 0 && t.Focus != t.Window {
		if err := SetFocusCrossThread(t.Thread, t.Focus); err != nil {
			// 控件可能已被销毁(比如切换了标签页),不算致命:
			// 顶层窗口已在前台,多数程序仍能正常接收粘贴
			return fmt.Errorf("还原控件焦点失败(窗口已在前台): %w", err)
		}
	}
	return nil
}
