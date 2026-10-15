package winapi

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procCreateMutexW        = kernel32.NewProc("CreateMutexW")
	procCreateEventW        = kernel32.NewProc("CreateEventW")
	procSetEvent            = kernel32.NewProc("SetEvent")
	procWaitForSingleObject = kernel32.NewProc("WaitForSingleObject")
	procCloseHandleWin      = kernel32.NewProc("CloseHandle")
)

const (
	ERROR_ALREADY_EXISTS = 183
	WAIT_OBJECT_0        = 0
	INFINITE             = 0xFFFFFFFF
)

// SingleInstance 用命名内核对象保证同一时刻只有一个实例在跑。
//
// 没有这层保护时,重复启动会产生多个进程:每个都有自己的托盘图标、
// 各自的剪贴板监听,而且只有第一个能注册上全局热键,其余只会报错。
// 用户看到的就是托盘里堆了一排图标,且退出其中一个也不影响其它。
type SingleInstance struct {
	name           string
	mutex          uintptr
	event          uintptr
	alreadyRunning bool
}

// AcquireSingleInstance 尝试取得单实例锁。
//
// 返回的 alreadyRunning=true 表示已有实例在运行:此时应调用
// NotifyExisting 唤醒它,然后自行退出。
func AcquireSingleInstance(name string) (*SingleInstance, error) {
	si := &SingleInstance{name: name}

	mutexName, err := windows.UTF16PtrFromString(`Local\` + name + `.mutex`)
	if err != nil {
		return nil, err
	}

	h, _, callErr := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(mutexName)))
	if h == 0 {
		return nil, fmt.Errorf("CreateMutex 失败: %w", callErr)
	}
	si.mutex = h

	// CreateMutex 成功但对象已存在时,GetLastError 为 ERROR_ALREADY_EXISTS
	if errno, ok := callErr.(windows.Errno); ok && errno == ERROR_ALREADY_EXISTS {
		si.alreadyRunning = true
		return si, nil
	}

	ev, err := openNamed(si.eventName())
	if err != nil {
		return nil, err
	}
	si.event = ev

	return si, nil
}

// AlreadyRunning 返回取得锁时是否已有实例在运行。
func (s *SingleInstance) AlreadyRunning() bool { return s.alreadyRunning }

func (s *SingleInstance) eventName() string     { return `Local\` + s.name + `.event` }
func (s *SingleInstance) quitEventName() string { return `Local\` + s.name + `.quit` }

// signalNamed 打开(或创建)命名事件并置位。
//
// 第二个实例没有持有事件句柄(取得锁时就提前返回了),只能按名字打开再置位。
func signalNamed(name string) error {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	h, _, openErr := procCreateEventW.Call(0, 0, 0, uintptr(unsafe.Pointer(namePtr)))
	if h == 0 {
		return fmt.Errorf("打开事件失败: %w", openErr)
	}
	defer procCloseHandleWin.Call(h)

	if ret, _, setErr := procSetEvent.Call(h); ret == 0 {
		return fmt.Errorf("SetEvent 失败: %w", setErr)
	}
	return nil
}

// openNamed 打开(或创建)命名事件,返回句柄。
func openNamed(name string) (uintptr, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	// 自动重置事件:一次通知只唤醒一次等待
	h, _, callErr := procCreateEventW.Call(0, 0, 0, uintptr(unsafe.Pointer(namePtr)))
	if h == 0 {
		return 0, fmt.Errorf("创建事件失败: %w", callErr)
	}
	return h, nil
}

// waitNamed 循环等待命名事件,每次触发调用一次 fn。stop 关闭后退出。
func waitNamed(handle uintptr, stop <-chan struct{}, fn func()) {
	if handle == 0 {
		return
	}
	go func() {
		for {
			ret, _, _ := procWaitForSingleObject.Call(handle, uintptr(INFINITE))
			if uint32(ret) != WAIT_OBJECT_0 {
				return
			}
			select {
			case <-stop:
				return
			default:
			}
			fn()
		}
	}()
}

// NotifyExisting 通知已在运行的实例"用户又启动了一次",通常让它把面板弹出来。
func (s *SingleInstance) NotifyExisting() error { return signalNamed(s.eventName()) }

// SignalQuit 通知已在运行的实例退出。供 --quit 与卸载脚本使用。
//
// 比 taskkill 好在走的是正常退出路径:能执行 Shutdown 清理、
// 注销热键、删除托盘图标。
func (s *SingleInstance) SignalQuit() error { return signalNamed(s.quitEventName()) }

// OnNotify 注册回调:每当有第二个实例启动时触发。
//
// 内部起 goroutine 阻塞等待事件,故回调不在消息线程上执行;
// 若回调要做 UI 操作,需自行切回合适的线程。stop 关闭后等待循环退出。
func (s *SingleInstance) OnNotify(stop <-chan struct{}, fn func()) {
	waitNamed(s.event, stop, fn)
}

// OnQuit 注册回调:收到退出信号时触发。
func (s *SingleInstance) OnQuit(stop <-chan struct{}, fn func()) {
	if s.alreadyRunning {
		return
	}
	h, err := openNamed(s.quitEventName())
	if err != nil {
		return
	}
	waitNamed(h, stop, fn)
}

// Close 释放内核对象。
func (s *SingleInstance) Close() {
	if s.event != 0 {
		procCloseHandleWin.Call(s.event)
		s.event = 0
	}
	if s.mutex != 0 {
		procCloseHandleWin.Call(s.mutex)
		s.mutex = 0
	}
}
