// Package winapi 封装 PasteWin 所需的 Win32 调用。
//
// 本包只做 syscall 封装,不含任何业务逻辑:不访问数据库、不发事件、不依赖 Wails。
// 上层服务(clipwatch / hotkey / paster)在此之上组装行为。
package winapi

// 窗口消息
const (
	WM_CREATE          = 0x0001
	WM_DESTROY         = 0x0002
	WM_CLOSE           = 0x0010
	WM_QUIT            = 0x0012
	WM_HOTKEY          = 0x0312
	WM_CLIPBOARDUPDATE = 0x031D
	WM_APP             = 0x8000
)

// 剪贴板格式。标准格式为预定义整数,注册格式("HTML Format" 等)需运行时查 ID。
const (
	CF_TEXT        = 1
	CF_BITMAP      = 2
	CF_DIB         = 8
	CF_UNICODETEXT = 13
	CF_HDROP       = 15
	CF_DIBV5       = 17
)

// 注册格式名。这些格式由 Windows 或应用在运行时注册,ID 不固定,需 RegisterClipboardFormat 查。
const (
	FmtHTML      = "HTML Format"
	FmtRTF       = "Rich Text Format"
	FmtPNG       = "PNG"

	// 密码管理器通行约定:应用把这两个格式写入剪贴板并置值为 0,
	// 表示"请勿记录到剪贴板历史"。见 DESIGN.md 4.3.2。
	FmtExcludeFromMonitor = "ExcludeClipboardContentFromMonitorProcessing"
	FmtCanIncludeInHistory = "CanIncludeInClipboardHistory"
)

// 全局热键修饰键
const (
	ModAlt      = 0x0001
	ModControl  = 0x0002
	ModShift    = 0x0004
	ModWin      = 0x0008
	ModNoRepeat = 0x4000
)

// 虚拟键码
const (
	VK_BACK    = 0x08
	VK_TAB     = 0x09
	VK_RETURN  = 0x0D
	VK_SHIFT   = 0x10
	VK_CONTROL = 0x11
	VK_MENU    = 0x12 // Alt
	VK_ESCAPE  = 0x1B
	VK_SPACE   = 0x20
	VK_LEFT    = 0x25
	VK_UP      = 0x26
	VK_RIGHT   = 0x27
	VK_DOWN    = 0x28
	VK_HOME    = 0x24
	VK_END     = 0x23
	VK_DELETE  = 0x2E
	VK_V       = 0x56
	VK_LWIN    = 0x5B
	VK_RWIN    = 0x5C
)

// SendInput 相关
const (
	INPUT_KEYBOARD   = 1
	KEYEVENTF_KEYUP  = 0x0002
	KEYEVENTF_UNICODE = 0x0004
	KEYEVENTF_SCANCODE = 0x0008
)

// 窗口显示状态
const (
	SW_HIDE        = 0
	SW_SHOWNORMAL  = 1
	SW_SHOW        = 5
	SW_RESTORE     = 9
	SW_SHOWNOACTIVATE = 4
)

// 窗口样式
const (
	WS_OVERLAPPED  = 0x00000000
	WS_POPUP       = 0x80000000
	WS_EX_TOOLWINDOW = 0x00000080 // 不出现在 Alt+Tab 和任务栏
	WS_EX_NOACTIVATE = 0x08000000
	WS_EX_TOPMOST    = 0x00000008
)

// 进程访问权限
const (
	PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
)

// 窗口置顶
var (
	HWND_TOPMOST   = ^uintptr(0)   // -1
	HWND_NOTOPMOST = ^uintptr(0) - 1 // -2
)
