package winapi

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	shell32dll = windows.NewLazySystemDLL("shell32.dll")

	procShellNotifyIconW = shell32dll.NewProc("Shell_NotifyIconW")
	procLoadImageW       = user32.NewProc("LoadImageW")
	procDestroyIcon      = user32.NewProc("DestroyIcon")

	procCreatePopupMenu = user32.NewProc("CreatePopupMenu")
	procAppendMenuW     = user32.NewProc("AppendMenuW")
	procTrackPopupMenu  = user32.NewProc("TrackPopupMenu")
	procDestroyMenu     = user32.NewProc("DestroyMenu")
)

// 托盘图标操作
const (
	NIM_ADD    = 0x00000000
	NIM_MODIFY = 0x00000001
	NIM_DELETE = 0x00000002

	NIF_MESSAGE = 0x00000001
	NIF_ICON    = 0x00000002
	NIF_TIP     = 0x00000004
)

// 托盘鼠标事件(回调消息的 lParam)
const (
	WM_LBUTTONUP     = 0x0202
	WM_LBUTTONDBLCLK = 0x0203
	WM_RBUTTONUP     = 0x0205
)

// 菜单标志
const (
	MF_STRING       = 0x00000000
	MF_SEPARATOR    = 0x00000800
	MF_GRAYED       = 0x00000001
	TPM_RIGHTBUTTON = 0x0002
	TPM_RETURNCMD   = 0x0100
)

// notifyIconDataW 对应 Win32 的 NOTIFYICONDATAW。
//
// x64 下的字段偏移(必须精确匹配,否则 Shell_NotifyIcon 会直接失败):
//
//	cbSize 0 / hWnd 8 / uID 16 / uFlags 20 / uCallbackMessage 24 / hIcon 32
//	szTip 40(128 宽字符)/ dwState 296 / dwStateMask 300
//	szInfo 304(256 宽字符)/ uVersion 816 / szInfoTitle 820(64 宽字符)
//	dwInfoFlags 948 / guidItem 952 / hBalloonIcon 968 —— 共 976 字节
type notifyIconDataW struct {
	cbSize           uint32
	_                uint32
	hWnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	_                uint32
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         [16]byte
	hBalloonIcon     uintptr
}

func newNotifyIconData(hwnd uintptr, id, callbackMsg uint32, hIcon uintptr, tip string) notifyIconDataW {
	nid := notifyIconDataW{
		cbSize:           uint32(unsafe.Sizeof(notifyIconDataW{})),
		hWnd:             hwnd,
		uID:              id,
		uFlags:           NIF_MESSAGE | NIF_ICON | NIF_TIP,
		uCallbackMessage: callbackMsg,
		hIcon:            hIcon,
	}
	copy(nid.szTip[:], windows.StringToUTF16(tip))
	return nid
}

// TrayIconAdd 在通知区域添加图标。
func TrayIconAdd(hwnd uintptr, id, callbackMsg uint32, hIcon uintptr, tip string) error {
	nid := newNotifyIconData(hwnd, id, callbackMsg, hIcon, tip)
	ret, _, callErr := procShellNotifyIconW.Call(NIM_ADD, uintptr(unsafe.Pointer(&nid)))
	if ret == 0 {
		return fmt.Errorf("Shell_NotifyIcon(NIM_ADD) 失败: %w", callErr)
	}
	return nil
}

// TrayIconDelete 移除通知区域图标。
func TrayIconDelete(hwnd uintptr, id uint32) error {
	nid := newNotifyIconData(hwnd, id, 0, 0, "")
	ret, _, callErr := procShellNotifyIconW.Call(NIM_DELETE, uintptr(unsafe.Pointer(&nid)))
	if ret == 0 {
		return fmt.Errorf("Shell_NotifyIcon(NIM_DELETE) 失败: %w", callErr)
	}
	return nil
}

// TrayIconSetTip 更新托盘图标的提示文字。
func TrayIconSetTip(hwnd uintptr, id uint32, tip string) error {
	nid := newNotifyIconData(hwnd, id, 0, 0, tip)
	nid.uFlags = NIF_TIP
	ret, _, callErr := procShellNotifyIconW.Call(NIM_MODIFY, uintptr(unsafe.Pointer(&nid)))
	if ret == 0 {
		return fmt.Errorf("Shell_NotifyIcon(NIM_MODIFY) 失败: %w", callErr)
	}
	return nil
}

// LoadIconFromFile 从 .ico 文件加载图标。调用方负责在不再使用时 DestroyIcon。
func LoadIconFromFile(path string, size int) (uintptr, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	const (
		IMAGE_ICON      = 1
		LR_LOADFROMFILE = 0x00000010
		LR_DEFAULTSIZE  = 0x00000040
	)
	h, _, callErr := procLoadImageW.Call(
		0,
		uintptr(unsafe.Pointer(pathPtr)),
		IMAGE_ICON,
		uintptr(size), uintptr(size),
		LR_LOADFROMFILE,
	)
	if h == 0 {
		return 0, fmt.Errorf("LoadImage 失败: %w", callErr)
	}
	return h, nil
}

// DestroyIconHandle 释放图标句柄。
func DestroyIconHandle(hIcon uintptr) {
	if hIcon != 0 {
		procDestroyIcon.Call(hIcon)
	}
}

// Menu 是 Win32 弹出菜单。
type Menu struct {
	handle uintptr
}

// NewMenu 创建一个空菜单。
func NewMenu() (*Menu, error) {
	h, _, callErr := procCreatePopupMenu.Call()
	if h == 0 {
		return nil, fmt.Errorf("CreatePopupMenu 失败: %w", callErr)
	}
	return &Menu{handle: h}, nil
}

// AddItem 追加一个菜单项。cmdID 为 0 时插入分隔线。
func (m *Menu) AddItem(cmdID uint32, label string) error {
	var labelPtr *uint16
	if label != "" {
		p, err := windows.UTF16PtrFromString(label)
		if err != nil {
			return err
		}
		labelPtr = p
	}
	flags := uintptr(MF_STRING)
	if cmdID == 0 {
		flags = MF_SEPARATOR
	}
	ret, _, callErr := procAppendMenuW.Call(
		m.handle, flags, uintptr(cmdID), uintptr(unsafe.Pointer(labelPtr)),
	)
	if ret == 0 {
		return fmt.Errorf("AppendMenuW 失败: %w", callErr)
	}
	return nil
}

// AddSeparator 插入分隔线。
func (m *Menu) AddSeparator() error { return m.AddItem(0, "") }

// Destroy 销毁菜单。
func (m *Menu) Destroy() {
	if m.handle != 0 {
		procDestroyMenu.Call(m.handle)
		m.handle = 0
	}
}

// TrackAtCursor 在鼠标位置弹出菜单,返回被选中的命令 ID(0 表示未选择)。
//
// 调用前必须先把本进程的某个窗口设为前台,否则菜单不会在点击别处时消失,
// 这是 TrackPopupMenu 的已知行为要求。
func (m *Menu) TrackAtCursor(hwnd uintptr) uint32 {
	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))

	// 不先设前台的话菜单会"粘"在屏幕上不消失
	procSetForegroundWindow.Call(hwnd)

	ret, _, _ := procTrackPopupMenu.Call(
		m.handle,
		TPM_RIGHTBUTTON|TPM_RETURNCMD,
		uintptr(pt.X), uintptr(pt.Y),
		0, hwnd, 0,
	)
	return uint32(ret)
}
