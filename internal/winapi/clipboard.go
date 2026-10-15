package winapi

import (
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procOpenClipboard                 = user32.NewProc("OpenClipboard")
	procCloseClipboard                = user32.NewProc("CloseClipboard")
	procEmptyClipboard                = user32.NewProc("EmptyClipboard")
	procGetClipboardData              = user32.NewProc("GetClipboardData")
	procSetClipboardData              = user32.NewProc("SetClipboardData")
	procIsClipboardFormatAvailable    = user32.NewProc("IsClipboardFormatAvailable")
	procEnumClipboardFormats          = user32.NewProc("EnumClipboardFormats")
	procRegisterClipboardFormatW      = user32.NewProc("RegisterClipboardFormatW")
	procGetClipboardSequenceNumber    = user32.NewProc("GetClipboardSequenceNumber")
	procGetClipboardOwner             = user32.NewProc("GetClipboardOwner")
	procAddClipboardFormatListener    = user32.NewProc("AddClipboardFormatListener")
	procRemoveClipboardFormatListener = user32.NewProc("RemoveClipboardFormatListener")

	procGlobalLock   = kernel32.NewProc("GlobalLock")
	procGlobalUnlock = kernel32.NewProc("GlobalUnlock")
	procGlobalSize   = kernel32.NewProc("GlobalSize")
	procGlobalAlloc  = kernel32.NewProc("GlobalAlloc")
	procGlobalFree   = kernel32.NewProc("GlobalFree")
)

const (
	GMEM_MOVEABLE = 0x0002
	GMEM_ZEROINIT = 0x0040
)

// OpenClipboardRetry 打开剪贴板,失败时退避重试。
//
// 剪贴板是全局独占资源,任何进程都可能短暂持有它。直接调用失败是常态而非异常,
// 因此这里必须重试,否则会随机丢事件。见 DESIGN.md 4.3.2。
func OpenClipboardRetry(owner uintptr, attempts int) error {
	var lastErr error
	for i := range attempts {
		ret, _, callErr := procOpenClipboard.Call(owner)
		if ret != 0 {
			return nil
		}
		lastErr = callErr
		// 线性退避:20ms, 40ms, 60ms ...
		time.Sleep(time.Duration(20*(i+1)) * time.Millisecond)
	}
	return fmt.Errorf("OpenClipboard 重试 %d 次仍失败: %w", attempts, lastErr)
}

// CloseClipboard 关闭剪贴板。
func CloseClipboard() {
	procCloseClipboard.Call()
}

// ClipboardSequenceNumber 返回剪贴板变更序号,每次内容变化自增。
// 用于判断内容是否真的变了(去重),以及识别自身写入造成的回环。
func ClipboardSequenceNumber() uint32 {
	ret, _, _ := procGetClipboardSequenceNumber.Call()
	return uint32(ret)
}

// ClipboardOwner 返回当前剪贴板内容的所有者窗口句柄,无主时为 0。
func ClipboardOwner() uintptr {
	ret, _, _ := procGetClipboardOwner.Call()
	return ret
}

// AddClipboardFormatListener 让 hwnd 接收 WM_CLIPBOARDUPDATE 通知。
func AddClipboardFormatListener(hwnd uintptr) error {
	ret, _, callErr := procAddClipboardFormatListener.Call(hwnd)
	if ret == 0 {
		return fmt.Errorf("AddClipboardFormatListener 失败: %w", callErr)
	}
	return nil
}

// RemoveClipboardFormatListener 注销剪贴板监听。
func RemoveClipboardFormatListener(hwnd uintptr) error {
	ret, _, callErr := procRemoveClipboardFormatListener.Call(hwnd)
	if ret == 0 {
		return fmt.Errorf("RemoveClipboardFormatListener 失败: %w", callErr)
	}
	return nil
}

// RegisterFormat 把格式名注册为剪贴板格式 ID。同名重复注册返回同一 ID,可安全反复调用。
func RegisterFormat(name string) (uint32, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	ret, _, _ := procRegisterClipboardFormatW.Call(uintptr(unsafe.Pointer(namePtr)))
	if ret == 0 {
		return 0, fmt.Errorf("RegisterClipboardFormat 失败: %s", name)
	}
	return uint32(ret), nil
}

// IsFormatAvailable 判断剪贴板当前是否含指定格式。
func IsFormatAvailable(format uint32) bool {
	ret, _, _ := procIsClipboardFormatAvailable.Call(uintptr(format))
	return ret != 0
}

// EnumFormats 枚举剪贴板当前所有格式 ID。调用前必须先打开剪贴板。
func EnumFormats() []uint32 {
	var formats []uint32
	var prev uint32
	for {
		ret, _, _ := procEnumClipboardFormats.Call(uintptr(prev))
		if ret == 0 {
			break
		}
		formats = append(formats, uint32(ret))
		prev = uint32(ret)
	}
	return formats
}

// readGlobal 把剪贴板数据句柄指向的全局内存整块拷出来。
//
// 必须拷贝而非直接引用:CloseClipboard 之后该内存归系统所有,指针随时失效。
//
// 这里用 ReadProcessMemory 读自己的进程,而不是把 GlobalLock 返回的 uintptr
// 转成 unsafe.Pointer 再 memcpy —— 后者会触发 go vet 的 unsafeptr 告警
// (syscall 返回的 uintptr 无法被静态证明有效),而走 API 既等价又无需 unsafe。
func readGlobal(hMem uintptr) ([]byte, error) {
	if hMem == 0 {
		return nil, fmt.Errorf("剪贴板数据句柄为空")
	}
	ptr, _, callErr := procGlobalLock.Call(hMem)
	if ptr == 0 {
		return nil, fmt.Errorf("GlobalLock 失败: %w", callErr)
	}
	defer procGlobalUnlock.Call(hMem)

	size, _, _ := procGlobalSize.Call(hMem)
	if size == 0 {
		return nil, fmt.Errorf("GlobalSize 返回 0")
	}

	buf := make([]byte, size)
	var read uintptr
	if err := windows.ReadProcessMemory(windows.CurrentProcess(), ptr, &buf[0], size, &read); err != nil {
		return nil, fmt.Errorf("读取剪贴板内存失败: %w", err)
	}
	if read != size {
		return nil, fmt.Errorf("剪贴板内存读取不完整: %d/%d 字节", read, size)
	}
	return buf, nil
}

// GetClipboardBytes 读取指定格式的原始字节。调用前必须先打开剪贴板。
func GetClipboardBytes(format uint32) ([]byte, error) {
	hMem, _, _ := procGetClipboardData.Call(uintptr(format))
	if hMem == 0 {
		return nil, fmt.Errorf("剪贴板无此格式: %d", format)
	}
	return readGlobal(hMem)
}

// SetClipboardBytes 以指定格式写入剪贴板。调用前必须先清空并持有剪贴板。
//
// SetClipboardData 成功后内存所有权移交系统,不能再 GlobalFree。
// 写入同样走 WriteProcessMemory,理由见 readGlobal。
func SetClipboardBytes(format uint32, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("拒绝写入空数据")
	}
	hMem, _, callErr := procGlobalAlloc.Call(GMEM_MOVEABLE|GMEM_ZEROINIT, uintptr(len(data)))
	if hMem == 0 {
		return fmt.Errorf("GlobalAlloc 失败: %w", callErr)
	}
	ptr, _, callErr := procGlobalLock.Call(hMem)
	if ptr == 0 {
		procGlobalFree.Call(hMem)
		return fmt.Errorf("GlobalLock 失败: %w", callErr)
	}

	var written uintptr
	err := windows.WriteProcessMemory(windows.CurrentProcess(), ptr, &data[0], uintptr(len(data)), &written)
	procGlobalUnlock.Call(hMem)
	if err != nil {
		procGlobalFree.Call(hMem)
		return fmt.Errorf("写入剪贴板内存失败: %w", err)
	}
	if written != uintptr(len(data)) {
		procGlobalFree.Call(hMem)
		return fmt.Errorf("剪贴板内存写入不完整: %d/%d 字节", written, len(data))
	}

	ret, _, callErr := procSetClipboardData.Call(uintptr(format), hMem)
	if ret == 0 {
		procGlobalFree.Call(hMem)
		return fmt.Errorf("SetClipboardData 失败: %w", callErr)
	}
	return nil
}

// EmptyClipboard 清空剪贴板。调用前必须先打开剪贴板。
func EmptyClipboard() error {
	ret, _, callErr := procEmptyClipboard.Call()
	if ret == 0 {
		return fmt.Errorf("EmptyClipboard 失败: %w", callErr)
	}
	return nil
}
