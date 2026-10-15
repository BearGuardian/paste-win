// Command hotkeyprobe 独立验证全局热键能否收到 WM_HOTKEY。
//
// 用来把"我们的注册/消息循环有问题"与"这个组合键被别的程序占用"
// 这两种情况区分开:它只用最少的代码注册一个热键并等待。
//
// 用法:
//
//	hotkeyprobe -keys ctrl+shift+v -duration 8s
//
// 退出码 0 表示收到了热键。
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"pastewin/internal/hotkey"
	"pastewin/internal/winapi"
)

func main() {
	keys := flag.String("keys", "ctrl+shift+v", "要注册的组合键")
	dur := flag.Duration("duration", 8*time.Second, "等待时长")
	flag.Parse()

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	accel, err := hotkey.Parse(*keys)
	if err != nil {
		fmt.Fprintf(os.Stderr, "解析组合键失败: %v\n", err)
		os.Exit(2)
	}

	got := make(chan struct{}, 1)
	mw, err := winapi.StartMessageWindow(func(hwnd uintptr, msg uint32, wParam, lParam uintptr) (uintptr, bool) {
		if msg == winapi.WM_HOTKEY {
			select {
			case got <- struct{}{}:
			default:
			}
			return 0, true
		}
		return 0, false
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建消息窗口失败: %v\n", err)
		os.Exit(1)
	}
	defer mw.Stop()

	// 与产品同样的路径:注册动作必须在消息窗口自己的线程上执行
	var regErr error
	if err := mw.RunOnThread(func() {
		regErr = winapi.RegisterHotKey(mw.HWND(), 1, accel.Modifiers, accel.VK)
	}); err != nil {
		fmt.Fprintf(os.Stderr, "RunOnThread 失败: %v\n", err)
		os.Exit(1)
	}
	if regErr != nil {
		fmt.Printf("注册失败: %v\n", regErr)
		os.Exit(1)
	}
	fmt.Printf("已注册 %s,等待 %v …\n", *keys, *dur)

	select {
	case <-got:
		fmt.Println("结果:收到 WM_HOTKEY ✓")
	case <-time.After(*dur):
		fmt.Println("结果:超时未收到 WM_HOTKEY ✗")
		os.Exit(1)
	}
}
