// Package hotkey 管理全局热键的注册与分发。
package hotkey

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"pastewin/internal/winapi"
)

// Accelerator 是一个解析后的热键组合。
type Accelerator struct {
	Modifiers uint32
	VK        uint16
	Raw       string
}

// Manager 在指定的消息窗口上注册热键,并把 WM_HOTKEY 分发给回调。
type Manager struct {
	win *winapi.MessageWindow

	mu       sync.Mutex
	handlers map[int]func()
	nextID   int
}

// New 创建管理器。热键必须注册到消息窗口,且注册动作会在该窗口的线程上执行
// (RegisterHotKey 要求调用线程拥有窗口)。
func New(win *winapi.MessageWindow) *Manager {
	return &Manager{
		win:      win,
		handlers: make(map[int]func()),
		nextID:   1,
	}
}

// Register 注册一个热键并返回其 ID。
func (m *Manager) Register(accel Accelerator, fn func()) (int, error) {
	m.mu.Lock()
	id := m.nextID
	m.nextID++
	m.mu.Unlock()

	var regErr error
	err := m.win.RunOnThread(func() {
		regErr = winapi.RegisterHotKey(m.win.HWND(), id, accel.Modifiers, accel.VK)
	})
	if err != nil {
		return 0, err
	}
	if regErr != nil {
		return 0, regErr
	}

	m.mu.Lock()
	m.handlers[id] = fn
	m.mu.Unlock()
	return id, nil
}

// Unregister 注销指定热键。
func (m *Manager) Unregister(id int) error {
	m.mu.Lock()
	_, ok := m.handlers[id]
	delete(m.handlers, id)
	m.mu.Unlock()

	if !ok {
		return nil
	}
	var unregErr error
	err := m.win.RunOnThread(func() {
		unregErr = winapi.UnregisterHotKey(m.win.HWND(), id)
	})
	if err != nil {
		return err
	}
	return unregErr
}

// Dispatch 处理一条 WM_HOTKEY。由消息窗口回调调用。
//
// 回调放到独立 goroutine 执行:唤起面板、注入按键都会阻塞几十毫秒,
// 直接在消息线程上跑会拖住剪贴板通知的处理。
func (m *Manager) Dispatch(id int) {
	m.mu.Lock()
	fn := m.handlers[id]
	m.mu.Unlock()

	if fn != nil {
		go fn()
	}
}

// Parse 解析 "Ctrl+Shift+V" 这样的组合键描述。
//
// 修饰键大小写不敏感;主键支持 A-Z、0-9、F1-F24,以及若干具名键。
func Parse(s string) (Accelerator, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Accelerator{}, fmt.Errorf("热键为空")
	}

	parts := strings.Split(raw, "+")
	var mods uint32
	var vk uint16

	for _, part := range parts {
		p := strings.TrimSpace(part)
		if p == "" {
			return Accelerator{}, fmt.Errorf("热键格式非法(出现空段): %q", raw)
		}

		switch strings.ToLower(p) {
		case "ctrl", "control":
			mods |= winapi.ModControl
			continue
		case "shift":
			mods |= winapi.ModShift
			continue
		case "alt":
			mods |= winapi.ModAlt
			continue
		case "win", "super", "meta", "cmd":
			mods |= winapi.ModWin
			continue
		}

		// 主键只能有一个
		if vk != 0 {
			return Accelerator{}, fmt.Errorf("热键含多个主键: %q", raw)
		}
		code, err := parseMainKey(p)
		if err != nil {
			return Accelerator{}, err
		}
		vk = code
	}

	if vk == 0 {
		return Accelerator{}, fmt.Errorf("热键缺少主键(只有修饰键): %q", raw)
	}
	// 无修饰键的全局热键会抢走所有普通按键,必须拒绝
	if mods == 0 {
		return Accelerator{}, fmt.Errorf("热键必须包含至少一个修饰键: %q", raw)
	}

	return Accelerator{Modifiers: mods, VK: vk, Raw: raw}, nil
}

func parseMainKey(p string) (uint16, error) {
	up := strings.ToUpper(p)

	if len(up) == 1 {
		c := up[0]
		switch {
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			return uint16(c), nil
		}
	}

	if strings.HasPrefix(up, "F") {
		if n, err := strconv.Atoi(up[1:]); err == nil && n >= 1 && n <= 24 {
			return uint16(0x70 + n - 1), nil // VK_F1 = 0x70
		}
	}

	named := map[string]uint16{
		"SPACE":  winapi.VK_SPACE,
		"TAB":    winapi.VK_TAB,
		"ENTER":  winapi.VK_RETURN,
		"RETURN": winapi.VK_RETURN,
		"ESC":    winapi.VK_ESCAPE,
		"ESCAPE": winapi.VK_ESCAPE,
		"BACK":   winapi.VK_BACK,
	}
	if code, ok := named[up]; ok {
		return code, nil
	}

	return 0, fmt.Errorf("无法识别的主键: %q", p)
}
