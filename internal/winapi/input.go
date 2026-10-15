package winapi

import (
	"fmt"
	"unsafe"
)

var (
	procSendInput        = user32.NewProc("SendInput")
	procGetAsyncKeyState = user32.NewProc("GetAsyncKeyState")
)

// VK_LBUTTON:鼠标左键。
const vkLButton = 0x01

// IsLeftButtonDown 报告鼠标左键当前是否按下。
//
// 用于面板的拖拽改高:前端 mousedown 后由 Go 侧轮询按键状态,
// 直到用户松手。比每次 mousemove 都过一趟 JS↔Go 桥要顺滑得多。
//
// 最高位为 1 表示当前按下(最低位是"自上次调用后是否按过",这里不用)。
func IsLeftButtonDown() bool {
	ret, _, _ := procGetAsyncKeyState.Call(vkLButton)
	return ret&0x8000 != 0
}

// keybdInput 对应 Win32 的 KEYBDINPUT。
// x64 下布局:WORD(2) + WORD(2) + DWORD(4) + DWORD(4) + 对齐填充(4) + ULONG_PTR(8) = 24 字节。
type keybdInput struct {
	wVk         uint16
	wScan       uint16
	dwFlags     uint32
	time        uint32
	dwExtraInfo uintptr
}

// input 对应 Win32 的 INPUT(type + union)。
//
// 关键细节:union 的大小由其中**最大**的成员决定,而不是 KEYBDINPUT。
// 三个成员里 MOUSEINPUT 是 32 字节(4+4+4+4+4+对齐填充4+ULONG_PTR8),
// 大于 KEYBDINPUT 的 24 字节和 HARDWAREINPUT 的 8 字节。
// 因此 sizeof(INPUT) 是 8 + 32 = 40 字节。
//
// 少补这 8 字节会让 SendInput 直接返回 ERROR_INVALID_PARAMETER,
// 所有按键注入全部失效。
type input struct {
	typ uint32
	_   uint32 // 对齐到 8 字节边界
	ki  keybdInput
	_   [8]byte // 补齐 union 到 MOUSEINPUT 的 32 字节
}

func sendInputs(inputs []input) error {
	if len(inputs) == 0 {
		return nil
	}
	ret, _, callErr := procSendInput.Call(
		uintptr(len(inputs)),
		uintptr(unsafe.Pointer(&inputs[0])),
		unsafe.Sizeof(input{}),
	)
	if ret != uintptr(len(inputs)) {
		return fmt.Errorf("SendInput 仅发送 %d/%d 个事件: %w", ret, len(inputs), callErr)
	}
	return nil
}

func keyEvent(vk uint16, keyUp bool) input {
	var flags uint32
	if keyUp {
		flags |= KEYEVENTF_KEYUP
	}
	return input{
		typ: INPUT_KEYBOARD,
		ki:  keybdInput{wVk: vk, dwFlags: flags},
	}
}

// SendCtrlV 合成一次 Ctrl+V,用于把剪贴板内容粘进目标窗口。
//
// 按下与抬起成对发送;若中途失败,已按下的修饰键会卡住,因此调用方
// 在返回错误后应把内容留在剪贴板里并提示用户手动粘贴。
func SendCtrlV() error {
	return sendInputs([]input{
		keyEvent(VK_CONTROL, false),
		keyEvent(VK_V, false),
		keyEvent(VK_V, true),
		keyEvent(VK_CONTROL, true),
	})
}

// SendCtrlDigit 合成 Ctrl+1..9,用于 Quick Paste。
func SendCtrlDigit(digit int) error {
	if digit < 1 || digit > 9 {
		return fmt.Errorf("Quick Paste 仅支持 1-9,收到 %d", digit)
	}
	vk := uint16('0' + digit)
	return sendInputs([]input{
		keyEvent(VK_CONTROL, false),
		keyEvent(vk, false),
		keyEvent(vk, true),
		keyEvent(VK_CONTROL, true),
	})
}

// SendEnter 合成一次回车。
func SendEnter() error {
	return sendInputs([]input{
		keyEvent(VK_RETURN, false),
		keyEvent(VK_RETURN, true),
	})
}

// SendEscape 合成一次 Esc,用于关闭目标窗口的补全/搜索框。
func SendEscape() error {
	return sendInputs([]input{
		keyEvent(VK_ESCAPE, false),
		keyEvent(VK_ESCAPE, true),
	})
}
