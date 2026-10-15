// Package paster 把历史条目写回剪贴板并粘进目标窗口。
//
// 这是整个应用最容易失败的一环:Windows 的前台锁定、剪贴板独占、
// 目标窗口在面板弹出期间被关闭,任何一种都会让"回车后什么都没发生"。
// 因此每一步都单独报错,并且保证内容一定已经进了剪贴板 —— 即使注入失败,
// 用户手动 Ctrl+V 也拿得到。
package paster

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"log"
	"os"
	"strings"
	"time"
	"unicode/utf16"

	"pastewin/internal/blobs"
	"pastewin/internal/model"
	"pastewin/internal/winapi"
)

const (
	// focusSettleDelay:焦点切换是异步的,ForceForeground 返回时窗口未必
	// 已经真正拿到输入焦点,不等一下就直接发按键会打到上一个前台窗口上。
	focusSettleDelay = 60 * time.Millisecond

	// panelHideDelay:WindowHide 是投递到主线程执行的,返回时窗口往往还没真正隐藏,
	// 不等一下的话按键会落到正在消失的面板上。
	panelHideDelay = 30 * time.Millisecond

	// interItemDelay:顺序粘贴时,给目标程序留出处理上一条的时间。
	interItemDelay = 120 * time.Millisecond
)

// Paster 执行回贴。
type Paster struct {
	blobs *blobs.Store

	// ignoreNextWrite 在写剪贴板前调用,让监听器跳过由此产生的变化,避免回环。
	ignoreNextWrite func()
}

// New 创建 Paster。
func New(b *blobs.Store) *Paster {
	return &Paster{blobs: b}
}

// SetIgnoreNextWrite 注入防回环回调,通常传 clipwatch.Watcher.IgnoreNextWrite。
func (p *Paster) SetIgnoreNextWrite(fn func()) { p.ignoreNextWrite = fn }

// Paste 完成一次回贴:写剪贴板 → 交还焦点 → 收起面板 → 注入 Ctrl+V。
//
// plainText 为真时只写纯文本(对应 Paste 的 "Paste as Plain Text")。
// hidePanel 由调用方传入(通常是 App.hidePanelWindow),可为 nil。
func (p *Paster) Paste(target winapi.FocusTarget, item model.Item, plainText bool, hidePanel func()) error {
	log.Printf("回贴开始: 条目=%s 类型=%s 纯文本=%v 目标窗口=%d(%s) 线程=%d 控件=%d(%s) 当前前台=%d",
		item.ID, item.Kind, plainText,
		target.Window, winapi.ClassName(target.Window),
		target.Thread,
		target.Focus, winapi.ClassName(target.Focus),
		winapi.GetForegroundWindow())

	if err := p.WriteToClipboard(item, plainText); err != nil {
		return fmt.Errorf("写入剪贴板失败: %w", err)
	}
	log.Printf("回贴: 剪贴板已就绪")

	if err := p.handOver(target, hidePanel); err != nil {
		return err
	}
	return p.inject()
}

// PasteSequence 顺序回贴多条(对应「粘贴栈」)。
//
// 焦点只交还一次:第一条之前就把前台还给目标窗口,后续各条直接注入,
// 中间留出间隔让目标程序把上一条处理完,否则后续按键会被吞掉。
func (p *Paster) PasteSequence(target winapi.FocusTarget, items []model.Item, hidePanel func()) error {
	if len(items) == 0 {
		return errors.New("粘贴队列为空")
	}

	if err := p.handOver(target, hidePanel); err != nil {
		return err
	}

	var failed []string
	for i, it := range items {
		if err := p.WriteToClipboard(it, false); err != nil {
			failed = append(failed, fmt.Sprintf("%q: %v", it.Preview, err))
			continue
		}
		if err := p.inject(); err != nil {
			failed = append(failed, fmt.Sprintf("%q: %v", it.Preview, err))
			continue
		}
		if i < len(items)-1 {
			time.Sleep(interItemDelay)
		}
	}

	if len(failed) > 0 {
		log.Printf("顺序粘贴有 %d 条失败: %s", len(failed), strings.Join(failed, "; "))
		return fmt.Errorf("顺序粘贴有 %d 条失败", len(failed))
	}
	return nil
}

// handOver 把前台交还给目标窗口,然后收起面板。
//
// **顺序不能反,这是回贴能否生效的关键。**
// 面板还在前台时,本进程就是"前台进程",SetForegroundWindow 才会被系统真正执行;
// 一旦先把面板藏起来,前台窗口就变成空(GetForegroundWindow 返回 0),
// 此时再调 SetForegroundWindow 会**返回成功但什么也不做** ——
// 实测日志里就是 `ret=1 → 前台=0`,连 AttachThreadInput 变通也救不回来。
// 这个顺序问题曾导致"面板看得见、点了却粘不出去,得手动再按一次 Ctrl+V"。
func (p *Paster) handOver(target winapi.FocusTarget, hidePanel func()) error {
	if target.Window == 0 {
		// 没有可还原的目标(例如从托盘直接唤起),内容已就位,交给用户手动粘贴
		log.Printf("回贴: 没有可还原的目标窗口,放弃注入")
		return fmt.Errorf("没有可回贴的目标窗口,内容已复制到剪贴板")
	}
	if !winapi.IsWindow(target.Window) {
		log.Printf("回贴: 目标窗口 %d 已销毁", target.Window)
		return fmt.Errorf("目标窗口已关闭,内容已复制到剪贴板")
	}

	// Restore 会一并把焦点还给窗口内部的控件 —— 只还原顶层窗口是不够的,
	// 用户原本在输入框里打字时,Ctrl+V 会落到顶层窗口上而静默失效。
	if err := target.Restore(); err != nil {
		log.Printf("回贴: 还原焦点失败: %v", err)
		return fmt.Errorf("切回目标窗口失败(%w),内容已复制到剪贴板", err)
	}
	log.Printf("回贴: 焦点已交还,当前前台=%d(%s)",
		winapi.GetForegroundWindow(), winapi.ClassName(winapi.GetForegroundWindow()))

	if hidePanel != nil {
		hidePanel()
	}
	// 等窗口真正隐藏、焦点落定,再注入按键
	time.Sleep(panelHideDelay + focusSettleDelay)
	return nil
}

// inject 发送一次 Ctrl+V,并记录按键会落到哪里。
func (p *Paster) inject() error {
	// 注入前把"按键到底会送到哪里"记下来。
	// 这条是排查回贴失效的关键:若前台窗口不是目标窗口,或焦点控件为空,
	// 那么 Ctrl+V 就算注入成功也粘不到输入框上,表现为"按了回车没反应"。
	fg := winapi.GetForegroundWindow()
	focus := winapi.FocusedWindow(0)
	log.Printf("回贴: 注入前 前台=%d(%s) 焦点控件=%d(%s)",
		fg, winapi.ClassName(fg), focus, winapi.ClassName(focus))

	if err := winapi.SendCtrlV(); err != nil {
		log.Printf("回贴: 注入失败: %v", err)
		return fmt.Errorf("发送 Ctrl+V 失败(%w),内容已复制到剪贴板", err)
	}
	log.Printf("回贴: Ctrl+V 已注入,注入后前台=%d", winapi.GetForegroundWindow())
	return nil
}

// WriteToClipboard 把条目写入系统剪贴板,不涉及焦点与按键。
//
// 托盘菜单、Quick Paste 之外的"仅复制"操作也走这里。
func (p *Paster) WriteToClipboard(item model.Item, plainText bool) error {
	if p.ignoreNextWrite != nil {
		p.ignoreNextWrite()
	}

	if err := winapi.OpenClipboardRetry(0, 5); err != nil {
		return err
	}
	defer winapi.CloseClipboard()

	if err := winapi.EmptyClipboard(); err != nil {
		return err
	}

	// 清空后按类型写入。多格式写入(富文本)时,
	// 纯文本副本必须一并写入,否则粘到不支持 HTML 的程序里会是空白。
	switch item.Kind {
	case model.KindText:
		return winapi.SetClipboardBytes(winapi.CF_UNICODETEXT, encodeUTF16(item.Content))

	case model.KindRichText:
		if plainText || item.Content == "" {
			return winapi.SetClipboardBytes(winapi.CF_UNICODETEXT, encodeUTF16(item.PlainText))
		}
		if err := winapi.SetClipboardBytes(winapi.CF_UNICODETEXT, encodeUTF16(item.PlainText)); err != nil {
			return err
		}
		id, err := winapi.RegisterFormat(winapi.FmtHTML)
		if err != nil {
			return err
		}
		return winapi.SetClipboardBytes(id, buildHTMLClipboard(item.Content))

	case model.KindImage:
		return p.writeImage(item)

	case model.KindFiles:
		return p.writeFiles(item)

	default:
		return fmt.Errorf("未知的条目类型: %s", item.Kind)
	}
}

func (p *Paster) writeImage(item model.Item) error {
	if item.BlobPath == "" {
		return fmt.Errorf("图片条目缺少文件路径")
	}
	raw, err := p.blobs.LoadPNG(item.BlobPath)
	if err != nil {
		return err
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("解码图片失败: %w", err)
	}
	return winapi.SetClipboardBytes(winapi.CF_DIB, winapi.EncodeDIB(img))
}

func (p *Paster) writeFiles(item model.Item) error {
	var paths []string
	if err := json.Unmarshal([]byte(item.Content), &paths); err != nil {
		return fmt.Errorf("解析文件路径失败: %w", err)
	}
	// 文件可能已被移动或删除,只保留仍然存在的
	alive := paths[:0]
	for _, path := range paths {
		if fileExists(path) {
			alive = append(alive, path)
		}
	}
	if len(alive) == 0 {
		return fmt.Errorf("原文件已不存在,内容无法回贴")
	}
	return winapi.SetClipboardBytes(winapi.CF_HDROP, buildHDROP(alive))
}

// encodeUTF16 生成 CF_UNICODETEXT 所需的 UTF-16LE + NUL 字节序列。
func encodeUTF16(s string) []byte {
	u := utf16.Encode([]rune(s))
	u = append(u, 0)
	buf := make([]byte, len(u)*2)
	for i, c := range u {
		binary.LittleEndian.PutUint16(buf[i*2:], c)
	}
	return buf
}

// buildHDROP 构造 CF_HDROP 数据:DROPFILES 头 + 双 NUL 结尾的宽字符路径列表。
func buildHDROP(paths []string) []byte {
	var buf bytes.Buffer

	header := make([]byte, 20)
	binary.LittleEndian.PutUint32(header[0:], 20) // pFiles:路径列表相对起点的偏移
	binary.LittleEndian.PutUint32(header[16:], 1) // fWide:TRUE
	buf.Write(header)

	for _, path := range paths {
		for _, c := range utf16.Encode([]rune(path)) {
			var b [2]byte
			binary.LittleEndian.PutUint16(b[:], c)
			buf.Write(b[:])
		}
		buf.Write([]byte{0, 0}) // 每条路径以 NUL 结尾
	}
	buf.Write([]byte{0, 0}) // 整个列表再以一个 NUL 收尾
	return buf.Bytes()
}

// buildHTMLClipboard 构造 Windows "HTML Format" 剪贴板数据。
//
// 格式要求一个纯文本头部声明各段偏移,偏移量是从数据起点算的字节位置。
// 头部用 %010d 定宽输出,这样无论偏移量多大,头部长度都是常量,
// 不需要迭代求解。
func buildHTMLClipboard(html string) []byte {
	const headerFmt = "Version:0.9\r\n" +
		"StartHTML:%010d\r\n" +
		"EndHTML:%010d\r\n" +
		"StartFragment:%010d\r\n" +
		"EndFragment:%010d\r\n"

	const prefix = "<html><body><!--StartFragment-->"
	const suffix = "<!--EndFragment--></body></html>"

	// 先算一次头部长度(用占位值),定宽格式保证结果与真实值无关
	headerLen := len(fmt.Sprintf(headerFmt, 0, 0, 0, 0))

	startHTML := headerLen
	startFragment := startHTML + len(prefix)
	endFragment := startFragment + len(html)
	endHTML := endFragment + len(suffix)

	var buf bytes.Buffer
	fmt.Fprintf(&buf, headerFmt, startHTML, endHTML, startFragment, endFragment)
	buf.WriteString(prefix)
	buf.WriteString(html)
	buf.WriteString(suffix)
	return buf.Bytes()
}

func fileExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}
