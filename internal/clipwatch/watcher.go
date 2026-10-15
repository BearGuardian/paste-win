// Package clipwatch 监听剪贴板变化,把内容解析成领域模型。
//
// 由 winapi.MessageWindow 的 WM_CLIPBOARDUPDATE 驱动,不做轮询。
package clipwatch

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image/png"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"pastewin/internal/model"
	"pastewin/internal/winapi"
)

const (
	// 单条内容的体量上限,超过直接丢弃 —— 防止把几百 MB 的位图灌进数据库
	maxTextBytes  = 8 << 20
	maxImageBytes = 32 << 20

	// 卡片摘要最多保留的字符数
	previewRunes = 200
)

// Watcher 监听剪贴板变化。
//
// 所有方法都可能被消息循环线程调用,内部状态用原子变量保护。
type Watcher struct {
	onCapture func(model.ClipContent)
	onSkip    func(reason string)

	// isExcluded 判断某来源应用是否在排除名单里,可为 nil
	isExcluded func(app string) bool

	lastSeq    atomic.Uint32
	ignoreNext atomic.Bool
	paused     atomic.Bool
}

// New 创建监听器。onCapture 在捕获到新内容时回调,onSkip 在主动跳过时回调(可为 nil)。
func New(onCapture func(model.ClipContent), onSkip func(reason string)) *Watcher {
	return &Watcher{onCapture: onCapture, onSkip: onSkip}
}

// SetExcluder 设置来源应用排除判定。
func (w *Watcher) SetExcluder(fn func(app string) bool) { w.isExcluded = fn }

// SetPaused 暂停/恢复捕获。
func (w *Watcher) SetPaused(paused bool) { w.paused.Store(paused) }

// IgnoreNextWrite 声明"接下来这次剪贴板变化是 PasteWin 自己写的"。
//
// 回贴时我们会把选中条目写回剪贴板,这会触发一次 WM_CLIPBOARDUPDATE。
// 若不屏蔽,历史里会凭空多出一条由自己产生的重复记录。
func (w *Watcher) IgnoreNextWrite() { w.ignoreNext.Store(true) }

// OnClipboardUpdate 处理一次 WM_CLIPBOARDUPDATE。由消息窗口回调调用。
func (w *Watcher) OnClipboardUpdate() {
	// 序号未变说明剪贴板内容其实没动(例如别的程序只是打开又关闭了它)
	seq := winapi.ClipboardSequenceNumber()
	if w.lastSeq.Swap(seq) == seq {
		return
	}

	// 自己写入造成的变化,消费掉标记后直接返回
	if w.ignoreNext.Swap(false) {
		return
	}

	if w.paused.Load() {
		w.skip("捕获已暂停")
		return
	}

	content, skipReason, err := w.read()
	if err != nil {
		w.skip(fmt.Sprintf("读取剪贴板失败: %v", err))
		return
	}
	if skipReason != "" {
		w.skip(skipReason)
		return
	}
	if content == nil {
		w.skip("剪贴板中没有可识别的内容")
		return
	}

	if w.onCapture != nil {
		w.onCapture(*content)
	}
}

func (w *Watcher) skip(reason string) {
	if w.onSkip != nil {
		w.onSkip(reason)
	}
}

// read 打开剪贴板并解析内容。
//
// 返回 (内容, 跳过原因, 错误)。跳过原因是"有意不记录"的说明,不是故障。
func (w *Watcher) read() (*model.ClipContent, string, error) {
	if err := winapi.OpenClipboardRetry(0, 5); err != nil {
		return nil, "", err
	}
	defer winapi.CloseClipboard()

	// 密码管理器等会显式标记"勿记录",这是最优先的跳过条件
	if winapi.HasSensitiveMark() {
		return nil, "来源应用标记为敏感内容", nil
	}

	src := winapi.GetClipboardSource()
	if w.isExcluded != nil && src.App != "" && w.isExcluded(src.App) {
		return nil, fmt.Sprintf("来源应用 %s 在排除名单中", src.App), nil
	}

	// 优先级:文件列表 > 位图 > 富文本 > 纯文本。
	// 从浏览器复制一段文字时剪贴板同时含 HTML 与纯文本,取更丰富的 HTML,
	// 用户想只要纯文本时可用「纯文本粘贴」。
	if winapi.IsFormatAvailable(winapi.CF_HDROP) {
		if c, err := readFiles(src); err == nil {
			return c, "", nil
		}
	}
	if winapi.IsFormatAvailable(winapi.CF_DIBV5) || winapi.IsFormatAvailable(winapi.CF_DIB) {
		if c, err := readImage(src); err == nil {
			return c, "", nil
		}
	}
	if c, err := readRichText(src); err == nil {
		return c, "", nil
	}
	if winapi.IsFormatAvailable(winapi.CF_UNICODETEXT) {
		if c, err := readText(src); err == nil {
			return c, "", nil
		}
	}
	return nil, "", nil
}

func readText(src winapi.ClipboardSource) (*model.ClipContent, error) {
	text, err := winapi.ReadText()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("文本为空")
	}
	if len(text) > maxTextBytes {
		return nil, fmt.Errorf("文本超过 %d 字节上限", maxTextBytes)
	}
	return &model.ClipContent{
		Kind:        model.KindText,
		Content:     text,
		PlainText:   text,
		Preview:     preview(text),
		Hash:        hashOf("text", []byte(text)),
		SourceApp:   src.App,
		SourceTitle: src.Title,
		ByteSize:    int64(len(text)),
	}, nil
}

// readRichText 尝试读 HTML;没有 HTML 时退回 RTF。两者都没有则返回错误,
// 让上层继续尝试纯文本。
func readRichText(src winapi.ClipboardSource) (*model.ClipContent, error) {
	// 纯文本副本始终保留:用于搜索,以及「纯文本粘贴」
	plain, _ := winapi.ReadText()

	html, err := winapi.ReadHTML()
	if err == nil && strings.TrimSpace(html) != "" {
		if len(html) > maxTextBytes {
			return nil, fmt.Errorf("HTML 超过 %d 字节上限", maxTextBytes)
		}
		// 有 HTML 但拿不到纯文本时,退而用 HTML 原文做预览
		display := plain
		if strings.TrimSpace(display) == "" {
			display = stripTags(html)
		}
		return &model.ClipContent{
			Kind:        model.KindRichText,
			Content:     html,
			PlainText:   plain,
			Preview:     preview(display),
			Hash:        hashOf("html", []byte(html)),
			SourceApp:   src.App,
			SourceTitle: src.Title,
			ByteSize:    int64(len(html)),
		}, nil
	}

	rtf, err := winapi.ReadRTF()
	if err == nil && strings.TrimSpace(rtf) != "" {
		if len(rtf) > maxTextBytes {
			return nil, fmt.Errorf("RTF 超过 %d 字节上限", maxTextBytes)
		}
		display := plain
		if strings.TrimSpace(display) == "" {
			display = "RTF 内容"
		}
		return &model.ClipContent{
			Kind:        model.KindRichText,
			Content:     rtf,
			PlainText:   plain,
			Preview:     preview(display),
			Hash:        hashOf("rtf", []byte(rtf)),
			SourceApp:   src.App,
			SourceTitle: src.Title,
			ByteSize:    int64(len(rtf)),
		}, nil
	}

	return nil, fmt.Errorf("剪贴板无富文本")
}

func readImage(src winapi.ClipboardSource) (*model.ClipContent, error) {
	// 优先 V5(V5 头里才有可信的 alpha 掩码)
	var raw []byte
	var err error
	if winapi.IsFormatAvailable(winapi.CF_DIBV5) {
		raw, err = winapi.GetClipboardBytes(winapi.CF_DIBV5)
	}
	if len(raw) == 0 {
		raw, err = winapi.GetClipboardBytes(winapi.CF_DIB)
	}
	if err != nil {
		return nil, err
	}

	img, err := winapi.DecodeDIB(raw)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("PNG 编码失败: %w", err)
	}
	if buf.Len() > maxImageBytes {
		return nil, fmt.Errorf("图片超过 %d 字节上限", maxImageBytes)
	}

	b := img.Bounds()
	return &model.ClipContent{
		Kind:        model.KindImage,
		ImagePNG:    buf.Bytes(),
		ImageW:      b.Dx(),
		ImageH:      b.Dy(),
		Preview:     fmt.Sprintf("图片 %d × %d", b.Dx(), b.Dy()),
		Hash:        hashOf("image", buf.Bytes()),
		SourceApp:   src.App,
		SourceTitle: src.Title,
		ByteSize:    int64(buf.Len()),
	}, nil
}

func readFiles(src winapi.ClipboardSource) (*model.ClipContent, error) {
	files, err := winapi.ReadFileList()
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("文件列表为空")
	}

	// 路径以 JSON 数组存库,便于前端直接解析
	payload, err := json.Marshal(files)
	if err != nil {
		return nil, err
	}

	var total int64
	for _, f := range files {
		total += int64(len(f))
	}

	return &model.ClipContent{
		Kind:        model.KindFiles,
		Content:     string(payload),
		PlainText:   strings.Join(files, "\n"),
		Files:       files,
		Preview:     previewFiles(files),
		Hash:        hashOf("files", payload),
		SourceApp:   src.App,
		SourceTitle: src.Title,
		ByteSize:    total,
	}, nil
}

func hashOf(prefix string, data []byte) string {
	h := sha256.New()
	h.Write([]byte(prefix))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// preview 生成卡片摘要:折叠空白、按字符数截断。
//
// 按 rune 而非 byte 截断,否则会在多字节字符中间切断产生乱码。
func preview(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= previewRunes {
		return s
	}
	r := []rune(s)
	return string(r[:previewRunes]) + "…"
}

func previewFiles(files []string) string {
	if len(files) == 1 {
		return baseName(files[0])
	}
	return fmt.Sprintf("%s 等 %d 个文件", baseName(files[0]), len(files))
}

func baseName(path string) string {
	if i := strings.LastIndexAny(path, `\/`); i >= 0 {
		return path[i+1:]
	}
	return path
}

// stripTags 粗暴地去掉 HTML 标签,仅用于没有纯文本副本时生成预览。
func stripTags(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '<':
			depth++
		case r == '>':
			if depth > 0 {
				depth--
			}
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}
