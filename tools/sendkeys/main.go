// Command sendkeys 是一个开发期小工具:合成任意组合键,用于端到端验证全局热键。
//
// 用法:
//
//	sendkeys ctrl+shift+v
//	sendkeys ctrl+v --delay 500ms
//
// 它不是产品的一部分,只在手工测试时使用。
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"pastewin/internal/winapi"
)

var user32 = windows.NewLazySystemDLL("user32.dll")

type keybdInput struct {
	wVk         uint16
	wScan       uint16
	dwFlags     uint32
	time        uint32
	dwExtraInfo uintptr
}

// input 必须与 Win32 的 INPUT 同布局:union 由最大的成员 MOUSEINPUT(32 字节)
// 决定,故总大小为 8 + 32 = 40。
type input struct {
	typ uint32
	_   uint32
	ki  keybdInput
	_   [8]byte
}

var procSendInput = user32.NewProc("SendInput")

func main() {
	delay := flag.Duration("delay", 300*time.Millisecond, "发送前的等待时间")
	setClip := flag.String("setclip", "", "先把文本写入剪贴板,不发送任何按键")
	flag.Parse()

	// 单独设置剪贴板。
	// 测试脚本里用 PowerShell 的 Set-Clipboard 会弹出控制台窗口抢走前台,
	// 导致被测窗口的前台状态不可控,所以这里自备一个不抢前台的写法。
	if *setClip != "" {
		if err := writeClipboard(*setClip); err != nil {
			fmt.Fprintf(os.Stderr, "写剪贴板失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("已写入剪贴板: %q\n", *setClip)
		return
	}

	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "用法: sendkeys [-delay 500ms] ctrl+shift+v")
		fmt.Fprintln(os.Stderr, "      sendkeys -setclip \"文本\"")
		os.Exit(2)
	}

	vks, err := parseChord(flag.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "解析失败: %v\n", err)
		os.Exit(2)
	}

	// 等一会儿再发,给调用方留出切换前台窗口的时间
	time.Sleep(*delay)

	inputs := make([]input, 0, len(vks)*2)
	for _, vk := range vks {
		inputs = append(inputs, input{typ: winapi.INPUT_KEYBOARD, ki: keybdInput{wVk: vk}})
	}
	for i := len(vks) - 1; i >= 0; i-- {
		inputs = append(inputs, input{typ: winapi.INPUT_KEYBOARD,
			ki: keybdInput{wVk: vks[i], dwFlags: winapi.KEYEVENTF_KEYUP}})
	}

	ret, _, err := procSendInput.Call(
		uintptr(len(inputs)),
		uintptr(unsafe.Pointer(&inputs[0])),
		unsafe.Sizeof(input{}),
	)
	if ret != uintptr(len(inputs)) {
		fmt.Fprintf(os.Stderr, "SendInput 失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("已发送 %s(%d 个按键事件)\n", flag.Arg(0), len(inputs))
}

// writeClipboard 用产品自己的剪贴板封装写入文本。
func writeClipboard(s string) error {
	u, _ := windows.UTF16FromString(s)
	buf := make([]byte, len(u)*2)
	for i, c := range u {
		buf[i*2] = byte(c)
		buf[i*2+1] = byte(c >> 8)
	}
	if err := winapi.OpenClipboardRetry(0, 5); err != nil {
		return err
	}
	defer winapi.CloseClipboard()
	if err := winapi.EmptyClipboard(); err != nil {
		return err
	}
	return winapi.SetClipboardBytes(winapi.CF_UNICODETEXT, buf)
}

func parseChord(s string) ([]uint16, error) {
	var out []uint16
	for _, part := range strings.Split(strings.ToLower(s), "+") {
		part = strings.TrimSpace(part)
		switch part {
		case "ctrl", "control":
			out = append(out, winapi.VK_CONTROL)
		case "shift":
			out = append(out, winapi.VK_SHIFT)
		case "alt":
			out = append(out, winapi.VK_MENU)
		case "win":
			out = append(out, winapi.VK_LWIN)
		case "esc", "escape":
			out = append(out, winapi.VK_ESCAPE)
		case "enter", "return":
			out = append(out, winapi.VK_RETURN)
		case "left":
			out = append(out, winapi.VK_LEFT)
		case "right":
			out = append(out, winapi.VK_RIGHT)
		case "up":
			out = append(out, winapi.VK_UP)
		case "down":
			out = append(out, winapi.VK_DOWN)
		case "home":
			out = append(out, winapi.VK_HOME)
		case "end":
			out = append(out, winapi.VK_END)
		case "delete", "del":
			out = append(out, winapi.VK_DELETE)
		case "backspace":
			out = append(out, winapi.VK_BACK)
		default:
			if len(part) == 1 {
				c := part[0]
				if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
					out = append(out, uint16(strings.ToUpper(part)[0]))
					continue
				}
			}
			return nil, fmt.Errorf("无法识别的键: %q", part)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("组合为空")
	}
	return out, nil
}
