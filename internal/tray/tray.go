// Package tray 实现通知区域(系统托盘)图标与右键菜单。
//
// 不用第三方托盘库:那些库要自己占一个 OS 线程跑消息循环,和 Wails 抢主线程。
// 这里复用 winapi.MessageWindow 已有的消息泵,通过回调消息接收鼠标事件。
package tray

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"

	"pastewin/internal/winapi"
)

// 菜单命令 ID
const (
	CmdTogglePanel = 1
	CmdPauseToggle = 2
	CmdSettings    = 3
	CmdOpenDataDir = 4
	CmdQuit        = 5
)

const (
	iconSize    = 32
	iconID      = 1
	callbackMsg = winapi.WM_APP + 2 // 托盘回调消息,与 RunOnThread 的自定义消息错开
)

// Tray 是托盘图标。
type Tray struct {
	hwnd      uintptr
	hIcon     uintptr
	iconPath  string
	tooltip   string
	paused    bool
	onCommand func(cmd uint32)
}

// New 创建并显示托盘图标。
//
// dataDir 用于存放运行时生成的 .ico;onCommand 在用户选择菜单项时回调。
func New(msgWindow *winapi.MessageWindow, dataDir, tooltip string, onCommand func(uint32)) (*Tray, error) {
	iconPath := filepath.Join(dataDir, "pastewin.ico")
	if err := ensureIconFile(iconPath); err != nil {
		return nil, err
	}

	hIcon, err := winapi.LoadIconFromFile(iconPath, iconSize)
	if err != nil {
		return nil, err
	}

	t := &Tray{
		hwnd:      msgWindow.HWND(),
		hIcon:     hIcon,
		iconPath:  iconPath,
		tooltip:   tooltip,
		onCommand: onCommand,
	}

	if err := winapi.TrayIconAdd(t.hwnd, iconID, callbackMsg, hIcon, tooltip); err != nil {
		winapi.DestroyIconHandle(hIcon)
		return nil, err
	}
	return t, nil
}

// HandleMessage 处理消息窗口转来的消息。返回 true 表示已处理。
//
// 由 main 的消息路由调用,只关心托盘回调消息。
func (t *Tray) HandleMessage(msg uint32, lParam uintptr) bool {
	if msg != callbackMsg {
		return false
	}
	switch uint32(lParam) {
	case winapi.WM_LBUTTONUP, winapi.WM_LBUTTONDBLCLK:
		t.send(CmdTogglePanel)
	case winapi.WM_RBUTTONUP:
		t.showMenu()
	}
	return true
}

func (t *Tray) showMenu() {
	menu, err := winapi.NewMenu()
	if err != nil {
		return
	}
	defer menu.Destroy()

	_ = menu.AddItem(CmdTogglePanel, "显示面板")

	pauseLabel := "暂停捕获"
	if t.paused {
		pauseLabel = "恢复捕获"
	}
	_ = menu.AddItem(CmdPauseToggle, pauseLabel)

	_ = menu.AddSeparator()
	_ = menu.AddItem(CmdSettings, "设置…")
	_ = menu.AddItem(CmdOpenDataDir, "打开数据目录")
	_ = menu.AddSeparator()
	_ = menu.AddItem(CmdQuit, "退出 PasteWin")

	if cmd := menu.TrackAtCursor(t.hwnd); cmd != 0 {
		t.send(cmd)
	}
}

func (t *Tray) send(cmd uint32) {
	if t.onCommand != nil {
		t.onCommand(cmd)
	}
}

// SetPaused 更新暂停状态,用于刷新菜单项文案。
func (t *Tray) SetPaused(paused bool) { t.paused = paused }

// SetTooltip 更新悬停提示。
func (t *Tray) SetTooltip(s string) {
	t.tooltip = s
	_ = winapi.TrayIconSetTip(t.hwnd, iconID, s)
}

// Close 移除托盘图标并释放资源。
func (t *Tray) Close() {
	_ = winapi.TrayIconDelete(t.hwnd, iconID)
	winapi.DestroyIconHandle(t.hIcon)
	t.hIcon = 0
}

// ensureIconFile 在文件不存在时生成托盘图标。
//
// 图标在运行时画出来而不是内嵌二进制资源:省掉一个 .ico 文件,
// 也避免构建流程里多一步资源嵌入。
func ensureIconFile(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}

	img := drawIcon(iconSize)
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		return fmt.Errorf("编码图标失败: %w", err)
	}

	if err := os.WriteFile(path, wrapICO(pngBuf.Bytes(), iconSize), 0o644); err != nil {
		return fmt.Errorf("写入图标文件失败: %w", err)
	}
	return nil
}

// drawIcon 画一个剪贴板形状的图标:圆角矩形主体 + 顶部夹子。
func drawIcon(size int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))

	body := color.RGBA{R: 0x0A, G: 0x84, B: 0xFF, A: 0xFF}
	clip := color.RGBA{R: 0xF2, G: 0xF2, B: 0xF7, A: 0xFF}

	s := float64(size)
	// 主体:圆角矩形
	left, top := s*0.16, s*0.20
	right, bottom := s*0.84, s*0.94
	radius := s * 0.16

	for y := range size {
		for x := range size {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			if inRoundedRect(fx, fy, left, top, right, bottom, radius) {
				img.SetRGBA(x, y, body)
			}
		}
	}

	// 顶部夹子:小圆角矩形
	cl, ct := s*0.36, s*0.08
	cr, cb := s*0.64, s*0.26
	cradius := s * 0.08
	for y := range size {
		for x := range size {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			if inRoundedRect(fx, fy, cl, ct, cr, cb, cradius) {
				img.SetRGBA(x, y, clip)
			}
		}
	}
	return img
}

// inRoundedRect 判断点是否落在圆角矩形内。
func inRoundedRect(px, py, left, top, right, bottom, radius float64) bool {
	if px < left || px > right || py < top || py > bottom {
		return false
	}
	// 四个角的圆心,分别判断到圆心的距离
	corners := [4][2]float64{
		{left + radius, top + radius},
		{right - radius, top + radius},
		{left + radius, bottom - radius},
		{right - radius, bottom - radius},
	}
	for _, c := range corners {
		dx, dy := px-c[0], py-c[1]
		// 只对"角所在的那个象限"做圆角裁切
		if math.Abs(dx) <= radius && math.Abs(dy) <= radius {
			if dx*dx+dy*dy > radius*radius {
				return false
			}
		}
	}
	return true
}

// wrapICO 把 PNG 数据包成单图标的 .ico 文件。
//
// Vista 之后 ICO 允许直接内嵌 PNG,无需再做 BMP 掩码。
func wrapICO(pngData []byte, size int) []byte {
	var buf bytes.Buffer

	// ICONDIR
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0)) // reserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // type: 1 = icon
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // count

	// ICONDIRENTRY(16 字节)
	buf.WriteByte(byte(size))                               // 宽(0 表示 256)
	buf.WriteByte(byte(size))                               // 高
	buf.WriteByte(0)                                        // 调色板颜色数
	buf.WriteByte(0)                                        // reserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))  // 颜色平面
	_ = binary.Write(&buf, binary.LittleEndian, uint16(32)) // 位深
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(pngData)))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(22)) // 图像数据偏移 = 6 + 16

	buf.Write(pngData)
	return buf.Bytes()
}
