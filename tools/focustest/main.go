// Command focustest 在单进程内验证"焦点还原 + 按键注入"这条链路。
//
// 为什么要单进程自测:外部脚本(bash / powershell)每次调用都会创建自己的窗口
// 并可能抢走前台,导致被测窗口的前台状态不可控,测试结果随机。
// 这里在同一个进程里建两个窗口,人为制造"目标窗口不是前台"的初始条件,
// 再调用产品的 winapi.ForceForeground + winapi.SendCtrlV,最后检查编辑框内容。
//
// 退出码 0 表示通过。
package main

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"pastewin/internal/winapi"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	pRegisterClassExW = user32.NewProc("RegisterClassExW")
	pCreateWindowExW  = user32.NewProc("CreateWindowExW")
	pDefWindowProcW   = user32.NewProc("DefWindowProcW")
	pShowWindow       = user32.NewProc("ShowWindow")
	pUpdateWindow     = user32.NewProc("UpdateWindow")
	pSetFocus         = user32.NewProc("SetFocus")
	pPeekMessageW     = user32.NewProc("PeekMessageW")
	pTranslateMessage = user32.NewProc("TranslateMessage")
	pDispatchMessageW = user32.NewProc("DispatchMessageW")
	pGetWindowTextW   = user32.NewProc("GetWindowTextW")
	pGetWindowTextLen = user32.NewProc("GetWindowTextLengthW")
	pGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	pGetForeground    = user32.NewProc("GetForegroundWindow")
	pGetFocus         = user32.NewProc("GetFocus")
)

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     windows.Handle
	hIcon         windows.Handle
	hCursor       windows.Handle
	hbrBackground windows.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       windows.Handle
}

type msgT struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
}

const (
	wsOverlappedWindow = 0x00CF0000
	wsVisible          = 0x10000000
	wsChild            = 0x40000000
	wsBorder           = 0x00800000
	wsVScroll          = 0x00200000
	esMultiline        = 0x0004
	esAutoVScroll      = 0x0040
	swShow             = 5
	pmRemove           = 0x0001
)

var (
	hInstance uintptr
	editHwnd  uintptr
)

func main() {
	// 窗口与消息泵必须同线程
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hInstance, _, _ = pGetModuleHandleW.Call(0)

	target := createWindow("PasteWinFocusTargetA", "目标窗口 A", 100, 100)
	decoy := createWindow("PasteWinFocusDecoyB", "干扰窗口 B", 500, 100)

	editHwnd = createEdit(target)
	if editHwnd == 0 {
		fail("创建编辑框失败")
	}

	pShowWindow.Call(target, swShow)
	pUpdateWindow.Call(target)
	pShowWindow.Call(decoy, swShow)
	pUpdateWindow.Call(decoy)
	pump(300)

	// 制造初始条件:让干扰窗口占据前台,目标窗口不是前台
	if err := winapi.ForceForeground(decoy); err != nil {
		fmt.Printf("准备阶段:把干扰窗口提到前台失败(可忽略): %v\n", err)
	}
	pump(400)

	if winapi.GetForegroundWindow() == target {
		fmt.Println("准备阶段:目标窗口仍是前台,无法验证焦点还原")
	} else {
		fmt.Println("准备阶段:目标窗口已非前台 ✓")
	}

	// 放一段标记文本到剪贴板,模拟 PasteWin 回贴前写入内容
	const marker = "FOCUS_TEST_OK_12345"
	if err := writeClipboardText(marker); err != nil {
		fail("写入剪贴板失败: %v", err)
	}

	// ---- 被测链路 ----
	if err := winapi.ForceForeground(target); err != nil {
		fail("ForceForeground 失败: %v", err)
	}
	if got := winapi.GetForegroundWindow(); got != target {
		fail("ForceForeground 返回成功后,前台窗口仍是 %d(期望 %d)", got, target)
	}
	// 必须把键盘焦点落到编辑框上:Ctrl+V 是发给"拥有焦点的控件"的,
	// 焦点若停在顶层窗口,编辑框收不到粘贴。
	pSetFocus.Call(editHwnd)
	pump(200)

	if focus, _, _ := pGetFocus.Call(); focus != editHwnd {
		fail("编辑框未取得焦点: 焦点=%d 编辑框=%d", focus, editHwnd)
	}
	fmt.Println("焦点已落到编辑框 ✓")

	if err := winapi.SendCtrlV(); err != nil {
		fail("SendCtrlV 失败: %v", err)
	}
	pump(600)

	got := readEditText()
	fmt.Printf("编辑框内容: %q\n", got)
	if got != marker {
		fail("注入内容不符: 期望 %q,实际 %q", marker, got)
	}

	fmt.Println("通过:焦点还原 + Ctrl+V 注入链路正常")

	// ---- 第二阶段:模拟真实场景 ----
	// 真实流程是:用户在目标窗口里(某个控件已有焦点)→ 面板抢走激活
	// → 回贴时还原目标窗口。要验证的是:还原后,那个控件焦点是否自动回来。
	// 如果不回来,Ctrl+V 就会落到顶层窗口上,粘贴静默失效。
	fmt.Println()
	fmt.Println("第二阶段:控件焦点在失去/恢复激活后是否保留")
	checkFocusRestoration(target, decoy)
}

func checkFocusRestoration(target, decoy uintptr) {
	// 让编辑框先拿到焦点,模拟"用户正在某个输入框里打字"
	if err := winapi.ForceForeground(target); err != nil {
		fail("还原目标窗口失败: %v", err)
	}
	pSetFocus.Call(editHwnd)
	pump(200)

	if focus, _, _ := pGetFocus.Call(); focus != editHwnd {
		fail("初始焦点未落到编辑框")
	}

	// 面板弹出前记录焦点目标(真实流程里由 ShowPanel 做)
	saved := winapi.CaptureFocusTarget()
	fmt.Printf("  已记录目标: 窗口=%d 控件=%d\n", saved.Window, saved.Focus)
	if saved.Focus != editHwnd {
		fail("记录的控件焦点不符: 得到 %d,期望 %d", saved.Focus, editHwnd)
	}

	// 面板抢走激活
	if err := winapi.ForceForeground(decoy); err != nil {
		fmt.Printf("  把干扰窗口提到前台失败(可忽略): %v\n", err)
	}
	pump(400)

	// 回贴时还原
	if err := saved.Restore(); err != nil {
		fail("Restore 失败: %v", err)
	}
	pump(300)

	focus, _, _ := pGetFocus.Call()
	if focus != editHwnd {
		fail("还原后编辑框仍未取得焦点(焦点=%d) —— 回贴会静默失效", focus)
	}
	fmt.Println("  结论:控件焦点已正确还原 ✓")

	// 再确认一次端到端:还原后注入 Ctrl+V 应当粘进编辑框
	if err := winapi.SendCtrlV(); err != nil {
		fail("SendCtrlV 失败: %v", err)
	}
	pump(600)
	if got := readEditText(); got == "" {
		fail("还原焦点后 Ctrl+V 仍未粘进编辑框")
	}
	fmt.Println("  结论:还原焦点后 Ctrl+V 正常落地 ✓")
}

func createWindow(className, title string, x, y int) uintptr {
	cn, _ := windows.UTF16PtrFromString(className)
	ti, _ := windows.UTF16PtrFromString(title)

	cb := syscall.NewCallback(func(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
		ret, _, _ := pDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
		return ret
	})

	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   cb,
		hInstance:     windows.Handle(hInstance),
		lpszClassName: cn,
	}
	if atom, _, err := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		fail("RegisterClassExW(%s) 失败: %v", className, err)
	}

	hwnd, _, err := pCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(cn)), uintptr(unsafe.Pointer(ti)),
		wsOverlappedWindow, uintptr(x), uintptr(y), 420, 220,
		0, 0, hInstance, 0,
	)
	if hwnd == 0 {
		fail("CreateWindowExW(%s) 失败: %v", className, err)
	}
	return hwnd
}

func createEdit(parent uintptr) uintptr {
	cls, _ := windows.UTF16PtrFromString("EDIT")
	hwnd, _, err := pCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(cls)), 0,
		wsChild|wsVisible|wsBorder|wsVScroll|esMultiline|esAutoVScroll,
		0, 0, 400, 180, parent, 0, hInstance, 0,
	)
	if hwnd == 0 {
		fmt.Fprintf(os.Stderr, "创建编辑框失败: %v\n", err)
	}
	return hwnd
}

// pump 处理一段时间内的消息,让窗口保持响应。
func pump(d time.Duration) {
	deadline := time.Now().Add(d)
	var msg msgT
	for time.Now().Before(deadline) {
		for {
			ret, _, _ := pPeekMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0, pmRemove)
			if ret == 0 {
				break
			}
			pTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
			pDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readEditText() string {
	n, _, _ := pGetWindowTextLen.Call(editHwnd)
	if n == 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	pGetWindowTextW.Call(editHwnd, uintptr(unsafe.Pointer(&buf[0])), n+1)
	return windows.UTF16ToString(buf)
}

// writeClipboardText 用产品自己的 API 写剪贴板,顺带验证写入路径。
func writeClipboardText(s string) error {
	u := utf16Encode(s)
	if err := winapi.OpenClipboardRetry(0, 5); err != nil {
		return err
	}
	defer winapi.CloseClipboard()
	if err := winapi.EmptyClipboard(); err != nil {
		return err
	}
	return winapi.SetClipboardBytes(winapi.CF_UNICODETEXT, u)
}

func utf16Encode(s string) []byte {
	u, _ := windows.UTF16FromString(s)
	buf := make([]byte, len(u)*2)
	for i, c := range u {
		buf[i*2] = byte(c)
		buf[i*2+1] = byte(c >> 8)
	}
	return buf
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "失败: "+format+"\n", args...)
	os.Exit(1)
}
