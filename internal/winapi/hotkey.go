package winapi

import "fmt"

var (
	procRegisterHotKey   = user32.NewProc("RegisterHotKey")
	procUnregisterHotKey = user32.NewProc("UnregisterHotKey")
)

// RegisterHotKey 注册全局热键。hwnd 收到的 WM_HOTKEY 消息里 wParam 即为 id。
//
// 注册是全局独占的:同一组合被别的进程占用时会失败,调用方需把错误暴露给用户,
// 而不是静默忽略 —— 否则用户会以为热键坏了。
func RegisterHotKey(hwnd uintptr, id int, modifiers uint32, vk uint16) error {
	ret, _, callErr := procRegisterHotKey.Call(hwnd, uintptr(id), uintptr(modifiers), uintptr(vk))
	if ret == 0 {
		return fmt.Errorf("RegisterHotKey 失败(组合可能已被其他程序占用): %w", callErr)
	}
	return nil
}

// UnregisterHotKey 注销热键。
func UnregisterHotKey(hwnd uintptr, id int) error {
	ret, _, callErr := procUnregisterHotKey.Call(hwnd, uintptr(id))
	if ret == 0 {
		return fmt.Errorf("UnregisterHotKey 失败: %w", callErr)
	}
	return nil
}
