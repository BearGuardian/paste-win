package winapi

import (
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ReadText 读取 CF_UNICODETEXT。调用前必须先打开剪贴板。
func ReadText() (string, error) {
	raw, err := GetClipboardBytes(CF_UNICODETEXT)
	if err != nil {
		return "", err
	}
	if len(raw) < 2 {
		return "", nil
	}
	// 按 UTF-16LE 解释,截到首个 NUL 为止
	u16 := unsafe.Slice((*uint16)(unsafe.Pointer(&raw[0])), len(raw)/2)
	for i, c := range u16 {
		if c == 0 {
			u16 = u16[:i]
			break
		}
	}
	return windows.UTF16ToString(u16), nil
}

// ReadFileList 读取 CF_HDROP(资源管理器里复制的文件/文件夹)。
//
// 数据布局:DROPFILES 头 + 双 NUL 结尾的宽字符路径列表。
//
//	DWORD pFiles   —— 路径列表相对数据起点的偏移
//	POINT pt       —— 8 字节
//	BOOL  fNC      —— 4 字节
//	BOOL  fWide    —— 4 字节,TRUE 表示宽字符
//
// 头部共 20 字节。
func ReadFileList() ([]string, error) {
	raw, err := GetClipboardBytes(CF_HDROP)
	if err != nil {
		return nil, err
	}
	const dropFilesSize = 20
	if len(raw) < dropFilesSize {
		return nil, fmt.Errorf("CF_HDROP 数据过短: %d 字节", len(raw))
	}

	pFiles := *(*uint32)(unsafe.Pointer(&raw[0]))
	fWide := *(*int32)(unsafe.Pointer(&raw[16]))

	if int(pFiles) >= len(raw) {
		return nil, fmt.Errorf("CF_HDROP 路径偏移越界: %d >= %d", pFiles, len(raw))
	}
	body := raw[pFiles:]

	if fWide == 0 {
		// 极罕见:ANSI 路径列表
		var out []string
		for _, s := range strings.Split(string(body), "\x00") {
			if s != "" {
				out = append(out, s)
			}
		}
		return out, nil
	}

	if len(body) < 2 {
		return nil, nil
	}
	u16 := unsafe.Slice((*uint16)(unsafe.Pointer(&body[0])), len(body)/2)

	var out []string
	start := 0
	for i, c := range u16 {
		if c != 0 {
			continue
		}
		if i > start {
			out = append(out, windows.UTF16ToString(u16[start:i]))
		}
		start = i + 1
	}
	return out, nil
}

// htmlHeader 是 Windows "HTML Format" 剪贴板数据的前缀。字段顺序不固定,
// 因此逐行扫描而不是按固定偏移切。
type htmlHeader struct {
	startHTML     int
	endHTML       int
	startFragment int
	endFragment   int
	headerEnd     int // 正文起点(第一个 "\r\n\r\n" 之后)
}

func parseHTMLHeader(s string) htmlHeader {
	h := htmlHeader{startHTML: -1, endHTML: -1, startFragment: -1, endFragment: -1}
	for _, line := range strings.Split(s, "\r\n") {
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			// 头部结束:进入 HTML 正文
			h.headerEnd = strings.Index(s, line)
			break
		}
		key := line[:colon]
		val := strings.TrimSpace(line[colon+1:])
		n := atoiSafe(val)
		switch key {
		case "StartHTML":
			h.startHTML = n
		case "EndHTML":
			h.endHTML = n
		case "StartFragment":
			h.startFragment = n
		case "EndFragment":
			h.endFragment = n
		}
	}
	return h
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	if s == "" || (s[0] < '0' || s[0] > '9') {
		return -1
	}
	return n
}

// ReadHTML 读取 "HTML Format" 并提取 fragment 部分(去掉 Windows 加的包装)。
// 调用前必须先打开剪贴板。
func ReadHTML() (string, error) {
	id, err := RegisterFormat(FmtHTML)
	if err != nil {
		return "", err
	}
	raw, err := GetClipboardBytes(id)
	if err != nil {
		return "", err
	}
	s := string(raw)
	h := parseHTMLHeader(s)

	// 优先取 fragment;缺失时退回整段 HTML
	start, end := h.startFragment, h.endFragment
	if start < 0 || end <= start || end > len(s) {
		start, end = h.startHTML, h.endHTML
	}
	if start < 0 || end <= start || end > len(s) {
		return strings.TrimSpace(s), nil
	}
	return strings.TrimSpace(s[start:end]), nil
}

// ReadRTF 读取 "Rich Text Format"。调用前必须先打开剪贴板。
func ReadRTF() (string, error) {
	id, err := RegisterFormat(FmtRTF)
	if err != nil {
		return "", err
	}
	raw, err := GetClipboardBytes(id)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// HasSensitiveMark 检查剪贴板是否被来源应用标记为"请勿记录"。
//
// 密码管理器(1Password、KeePass 等)会写入 ExcludeClipboardContentFromMonitorProcessing
// 或 CanIncludeInClipboardHistory 并置值为 0。见 DESIGN.md 4.3.2。
// 调用前必须先打开剪贴板。
func HasSensitiveMark() bool {
	for _, name := range []string{FmtExcludeFromMonitor, FmtCanIncludeInHistory} {
		id, err := RegisterFormat(name)
		if err != nil {
			continue
		}
		if !IsFormatAvailable(id) {
			continue
		}
		raw, err := GetClipboardBytes(id)
		if err != nil {
			continue
		}
		// 值为单个 DWORD 0 表示"不要记录"
		if len(raw) >= 4 && *(*uint32)(unsafe.Pointer(&raw[0])) == 0 {
			return true
		}
	}
	return false
}
