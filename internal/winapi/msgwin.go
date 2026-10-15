package winapi

import (
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 系统 DLL 过程。统一用 LazyDLL,首次调用时解析。
var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")

	procRegisterClassExW = user32.NewProc("RegisterClassExW")
	procCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procDestroyWindow    = user32.NewProc("DestroyWindow")
	procGetMessageW      = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessageW = user32.NewProc("DispatchMessageW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procPostMessageW     = user32.NewProc("PostMessageW")
	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
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

// MsgHandler 处理窗口消息。返回 handled=false 时交由 DefWindowProc 处理。
type MsgHandler func(hwnd uintptr, msg uint32, wParam, lParam uintptr) (result uintptr, handled bool)

// 窗口过程由系统在消息循环线程回调,必须是一个长期存活的包级函数。
//
// 不能按窗口实例注册回调:若两个实例共用同一个窗口类(类只能注册一次),
// 第二个实例注册的窗口收到的消息仍会打到第一个实例的回调上,而那个实例可能已被回收。
// 因此这里用包级回调 + hwnd 分发表。
var (
	wndProcCb = syscall.NewCallback(messageWindowProc)

	dispatchMu sync.RWMutex
	dispatch   = map[uintptr]*MessageWindow{}

	classOnce sync.Once
	classErr  error
)

const messageWindowClass = "PasteWin.MessageWindow"

// wmRunOnThread 是自定义消息,用于把闭包投递到消息循环线程执行。
const wmRunOnThread = WM_APP + 1

func messageWindowProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	dispatchMu.RLock()
	w := dispatch[hwnd]
	dispatchMu.RUnlock()

	if w != nil {
		// 内部消息优先:这些是 RunOnThread 投递过来的任务,必须在消息线程上跑完
		if msg == wmRunOnThread {
			w.drainTasks()
			return 0
		}
		if w.onMsg != nil {
			if result, handled := w.onMsg(hwnd, msg, wParam, lParam); handled {
				return result
			}
		}
	}
	ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
	return ret
}

// MessageWindow 是一个不可见的消息接收窗口。
//
// 它存在的意义:剪贴板通知(WM_CLIPBOARDUPDATE)和全局热键(WM_HOTKEY)都只能投递到
// 某个具体窗口,而 Wails 自己的窗口不适合挂这些消息(会和它的生命周期耦合)。
// 这里自建专用窗口 + 独立消息循环,与 Wails 完全隔离。
type MessageWindow struct {
	hwnd  uintptr
	onMsg MsgHandler

	// tasks 承载 RunOnThread 投递的闭包,由消息循环线程消费
	tasks chan func()

	ready  chan error
	exited chan struct{}
}

// StartMessageWindow 创建隐藏窗口并启动消息循环。
//
// 消息循环跑在专用 OS 线程上(独立于 Wails 主线程的循环)。函数返回时窗口已创建完毕,
// 可安全取 HWND() 做 AddClipboardFormatListener / RegisterHotKey。
func StartMessageWindow(onMsg MsgHandler) (*MessageWindow, error) {
	w := &MessageWindow{
		onMsg:  onMsg,
		tasks:  make(chan func(), 16),
		ready:  make(chan error, 1),
		exited: make(chan struct{}),
	}

	go func() {
		// 消息循环必须绑定固定 OS 线程:窗口属于创建它的线程,换线程就收不到消息。
		// 主线程已被 Wails 的 LockOSThread 独占,这里会拿到一个新的 M。
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		w.run()
	}()

	if err := <-w.ready; err != nil {
		return nil, err
	}
	return w, nil
}

func (w *MessageWindow) run() {
	defer close(w.exited)

	if err := registerMessageClass(); err != nil {
		w.ready <- err
		return
	}

	hInstance, _, _ := procGetModuleHandleW.Call(0)
	className, _ := windows.UTF16PtrFromString(messageWindowClass)

	hwnd, _, callErr := procCreateWindowExW.Call(
		WS_EX_TOOLWINDOW|WS_EX_NOACTIVATE, // 不进 Alt+Tab,不抢焦点
		uintptr(unsafe.Pointer(className)),
		0,       // 无标题
		WS_POPUP, // 无边框
		0, 0, 0, 0,
		0, // 无父窗口
		0, // 无菜单
		hInstance,
		0,
	)
	if hwnd == 0 {
		w.ready <- fmt.Errorf("CreateWindowExW 失败: %w", callErr)
		return
	}
	w.hwnd = hwnd

	dispatchMu.Lock()
	dispatch[hwnd] = w
	dispatchMu.Unlock()

	w.ready <- nil

	// 标准消息泵
	var msg msgT
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		// 返回 0 = WM_QUIT,返回 -1 = 错误;两者都退出循环
		if int32(ret) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}

	dispatchMu.Lock()
	delete(dispatch, hwnd)
	dispatchMu.Unlock()
}

func registerMessageClass() error {
	classOnce.Do(func() {
		className, err := windows.UTF16PtrFromString(messageWindowClass)
		if err != nil {
			classErr = err
			return
		}
		hInstance, _, _ := procGetModuleHandleW.Call(0)

		wc := wndClassExW{
			cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
			lpfnWndProc:   wndProcCb,
			hInstance:     windows.Handle(hInstance),
			lpszClassName: className,
		}
		atom, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
		if atom == 0 {
			classErr = fmt.Errorf("RegisterClassExW 失败: %w", callErr)
		}
	})
	return classErr
}

// HWND 返回窗口句柄,用于 AddClipboardFormatListener / RegisterHotKey。
func (w *MessageWindow) HWND() uintptr { return w.hwnd }

// Post 向本窗口投递一条自定义消息(供其他 goroutine 唤醒消息循环)。
func (w *MessageWindow) Post(msg uint32, wParam, lParam uintptr) error {
	ret, _, callErr := procPostMessageW.Call(w.hwnd, uintptr(msg), wParam, lParam)
	if ret == 0 {
		return fmt.Errorf("PostMessageW 失败: %w", callErr)
	}
	return nil
}

// RunOnThread 在消息循环所在线程上同步执行 fn,并等待其返回。
//
// 有些 Win32 调用要求"调用线程必须拥有该窗口",最典型的是 RegisterHotKey:
// 从别的线程调用会直接失败并报 "Invalid window; it belongs to other thread"。
// 这类调用必须通过本方法投递到消息线程上执行。
func (w *MessageWindow) RunOnThread(fn func()) error {
	done := make(chan struct{})
	select {
	case w.tasks <- func() {
		defer close(done)
		fn()
	}:
	case <-w.exited:
		return fmt.Errorf("消息窗口已关闭")
	}

	if err := w.Post(wmRunOnThread, 0, 0); err != nil {
		return err
	}
	<-done
	return nil
}

// drainTasks 把队列里积压的任务全部执行完。由消息线程调用。
func (w *MessageWindow) drainTasks() {
	for {
		select {
		case fn := <-w.tasks:
			fn()
		default:
			return
		}
	}
}

// Stop 销毁窗口并等待消息循环退出。
func (w *MessageWindow) Stop() {
	if w.hwnd == 0 {
		return
	}
	procDestroyWindow.Call(w.hwnd)
	procPostQuitMessage.Call(0)
	<-w.exited
}
