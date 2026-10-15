// Command keytarget 是一个测试靶子:开一个带文本编辑框的窗口,取得前台焦点,
// 然后持续把编辑框内容写到文件。
//
// 用于验证 PasteWin 的"回贴"链路(焦点还原 + SendInput 注入)是否真的
// 把按键送到了目标控件。PowerShell 的 WinForms 在本机加载不了,故用 Go 写。
//
// 全部动作都在同一个锁定的 OS 线程上完成:窗口属于创建它的线程,
// 消息泵与 GetWindowText 都必须在那个线程上,否则读不到内容或收不到消息。
//
// 用法:
//
//	keytarget -out result.txt
//
// 进程一直运行,由外部终止。
package main

import (
	"flag"
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

	pRegisterClassExW    = user32.NewProc("RegisterClassExW")
	pCreateWindowExW     = user32.NewProc("CreateWindowExW")
	pDefWindowProcW      = user32.NewProc("DefWindowProcW")
	pShowWindow          = user32.NewProc("ShowWindow")
	pUpdateWindow        = user32.NewProc("UpdateWindow")
	pSetFocus            = user32.NewProc("SetFocus")
	pPeekMessageW        = user32.NewProc("PeekMessageW")
	pTranslateMessage    = user32.NewProc("TranslateMessage")
	pDispatchMessageW    = user32.NewProc("DispatchMessageW")
	pGetWindowTextW      = user32.NewProc("GetWindowTextW")
	pGetWindowTextLen    = user32.NewProc("GetWindowTextLengthW")
	pGetModuleHandleW    = kernel32.NewProc("GetModuleHandleW")
	pGetForegroundWindow = user32.NewProc("GetForegroundWindow")
	pGetFocus            = user32.NewProc("GetFocus")
	pGetClassNameW       = user32.NewProc("GetClassNameW")
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
	wmDestroy          = 0x0002
	wmSetFocus         = 0x0007
	pmRemove           = 0x0001
)

var editHwnd uintptr

func main() {
	out := flag.String("out", "keytarget_result.txt", "结果输出文件")
	hold := flag.Duration("hold", 0, "持续占据前台的时长(0 表示只在启动时抢一次)")
	flag.Parse()

	// 窗口属于创建它的线程;不锁线程的话消息泵会跑在别的 OS 线程上,
	// 窗口永远收不到消息,GetWindowText 也读不到内容。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	className, _ := windows.UTF16PtrFromString("PasteWinKeyTarget")
	title, _ := windows.UTF16PtrFromString("PasteWin 回贴测试靶子")
	editClass, _ := windows.UTF16PtrFromString("EDIT")
	hInstance, _, _ := pGetModuleHandleW.Call(0)

	cb := syscall.NewCallback(func(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
		if msg == wmDestroy {
			os.Exit(0)
		}
		// 真实程序在窗口重新获得激活时会把键盘焦点还给自己的输入控件。
		// 裸的 Win32 窗口不会(DefWindowProc 不做这件事),不补这一步,
		// 靶子就无法区分"产品没还原控件焦点"与"靶子自己不还原焦点"。
		if msg == wmSetFocus && editHwnd != 0 {
			pSetFocus.Call(editHwnd)
		}
		ret, _, _ := pDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
		return ret
	})

	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   cb,
		hInstance:     windows.Handle(hInstance),
		lpszClassName: className,
	}
	if atom, _, err := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		fmt.Fprintf(os.Stderr, "RegisterClassExW 失败: %v\n", err)
		os.Exit(1)
	}

	hwnd, _, err := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		wsOverlappedWindow,
		200, 200, 700, 400,
		0, 0, hInstance, 0,
	)
	if hwnd == 0 {
		fmt.Fprintf(os.Stderr, "CreateWindowExW 失败: %v\n", err)
		os.Exit(1)
	}

	editHwnd, _, err = pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(editClass)),
		0,
		wsChild|wsVisible|wsBorder|wsVScroll|esMultiline|esAutoVScroll,
		0, 0, 680, 350,
		hwnd, 0, hInstance, 0,
	)
	if editHwnd == 0 {
		fmt.Fprintf(os.Stderr, "创建编辑框失败: %v\n", err)
		os.Exit(1)
	}

	pShowWindow.Call(hwnd, swShow)
	pUpdateWindow.Call(hwnd)

	// 由别的进程(如 shell)启动时,直接 SetForegroundWindow 会被 Windows
	// 的前台锁定挡掉。复用产品里的 ForceForeground 走 AttachThreadInput 变通。
	if err := winapi.ForceForeground(hwnd); err != nil {
		fmt.Fprintf(os.Stderr, "取得前台焦点失败: %v\n", err)
	}
	pSetFocus.Call(editHwnd)

	// PeekMessage + 定时轮询:所有调用都留在窗口所属线程上,
	// 既保证消息被处理,又保证 GetWindowText 能安全读到内容。
	var msg msgT
	lastWrite := time.Time{}
	lastText := ""
	lastStatus := time.Time{}
	lastGrab := time.Time{}
	holdUntil := time.Now().Add(*hold)
	for {
		for {
			ret, _, _ := pPeekMessageW.Call(
				uintptr(unsafe.Pointer(&msg)), 0, 0, 0, pmRemove,
			)
			if ret == 0 {
				break
			}
			pTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
			pDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
		}

		if time.Since(lastWrite) > 300*time.Millisecond {
			lastWrite = time.Now()
			text := readEditText()
			if text != lastText {
				lastText = text
				fmt.Fprintf(os.Stderr, "[靶子] 内容变化: %q\n", text)
			}
			if err := os.WriteFile(*out, []byte(text), 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "写结果失败: %v\n", err)
			}
		}

		// 持续把前台抢回来。自动化测试期间终端、编辑器会不断抢走焦点,
		// 靶子必须稳住前台,否则"面板捕获到的目标"根本不是靶子,测不出结果。
		//
		// 唯一的例外是 PasteWin 的面板:它抢前台是为了接收键盘输入,
		// 靶子再去抢回来会触发面板的"失去焦点自动收起",测试就永远走不到回贴那一步。
		if holdUntil.After(time.Now()) && time.Since(lastGrab) > 200*time.Millisecond {
			lastGrab = time.Now()
			fg, _, _ := pGetForegroundWindow.Call()
			if fg != hwnd && classNameOf(fg) != "wailsWindow" {
				_ = winapi.ForceForeground(hwnd)
			}
		}

		// 定期报告前台/焦点状态,便于判断注入的按键到底该落到哪
		if time.Since(lastStatus) > 2*time.Second {
			lastStatus = time.Now()
			fg, _, _ := pGetForegroundWindow.Call()
			focus, _, _ := pGetFocus.Call()
			fmt.Fprintf(os.Stderr, "[靶子] 前台=%d 自身=%d 焦点控件=%d 编辑框=%d\n",
				fg, hwnd, focus, editHwnd)
		}
		time.Sleep(15 * time.Millisecond)
	}
}

// classNameOf 返回窗口类名,取不到返回空串。
func classNameOf(h uintptr) string {
	if h == 0 {
		return ""
	}
	buf := make([]uint16, 256)
	n, _, _ := pGetClassNameW.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
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
