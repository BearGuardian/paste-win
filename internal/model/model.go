// Package model 定义跨包共享的领域类型。
//
// 单独成包是为了避免 store / clipwatch / paster 之间互相引用形成环。
// 这里只放数据结构,不放行为。
package model

import "time"

// Kind 是剪贴板条目的内容类型。
type Kind string

const (
	KindText     Kind = "text"     // 纯文本
	KindImage    Kind = "image"    // 位图
	KindFiles    Kind = "files"    // 文件/文件夹列表(CF_HDROP)
	KindRichText Kind = "richtext" // 带格式文本(HTML / RTF)
)

// Valid 判断是否为已知类型,用于校验来自前端的过滤条件。
func (k Kind) Valid() bool {
	switch k {
	case KindText, KindImage, KindFiles, KindRichText:
		return true
	}
	return false
}

// Item 是一条剪贴板历史记录。
type Item struct {
	ID          string    `json:"id"`
	Kind        Kind      `json:"kind"`
	Content     string    `json:"content"`     // 主载荷:文本正文 / HTML 源 / 文件路径 JSON 数组
	PlainText   string    `json:"plainText"`   // 纯文本表示,供搜索与「纯文本粘贴」使用
	Preview     string    `json:"preview"`     // 卡片上显示的摘要
	BlobPath    string    `json:"blobPath"`    // 图片 PNG 相对路径
	ThumbPath   string    `json:"thumbPath"`   // 缩略图相对路径
	SourceApp   string    `json:"sourceApp"`   // 来源进程名
	SourceTitle string    `json:"sourceTitle"` // 来源窗口标题
	Hash        string    `json:"hash"`        // 内容指纹,用于去重
	Pinned      bool      `json:"pinned"`
	PinboardID  string    `json:"pinboardId"`
	CreatedAt   time.Time `json:"createdAt"`
	ByteSize    int64     `json:"byteSize"`
}

// ClipContent 是刚从剪贴板解析出来的原始内容,尚未落库。
type ClipContent struct {
	Kind        Kind
	Content     string   // 主载荷
	PlainText   string   // 纯文本表示
	Files       []string // KindFiles 时的路径列表
	ImagePNG    []byte   // KindImage 时的 PNG 编码
	ImageW      int
	ImageH      int
	Preview     string
	Hash        string
	SourceApp   string
	SourceTitle string
	ByteSize    int64
}

// Pinboard 是长期保存的分组(Paste 里的 Pinboard)。
type Pinboard struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	SortOrder int       `json:"sortOrder"`
	CreatedAt time.Time `json:"createdAt"`
	ItemCount int       `json:"itemCount"`
}

// Filter 是列表查询条件。零值表示不过滤。
type Filter struct {
	Keyword    string `json:"keyword"`    // 在正文与来源应用中模糊匹配
	Kind       Kind   `json:"kind"`       // 按内容类型
	PinboardID string `json:"pinboardId"` // 指定分组;"" 表示全部
	SourceApp  string `json:"sourceApp"`  // 按来源应用
	Since      int64  `json:"since"`      // Unix 毫秒,只看此时间之后
	Until      int64  `json:"until"`      // Unix 毫秒,只看此时间之前
	Limit      int    `json:"limit"`
	Offset     int    `json:"offset"`
}

// Settings 是用户可配置项。
type Settings struct {
	Hotkey        string   `json:"hotkey"`        // 全局热键,如 "Ctrl+Shift+V"
	MaxItems      int      `json:"maxItems"`      // 历史上限,超出后清理未 Pin 的最旧条目
	ExcludedApps  []string `json:"excludedApps"`  // 不记录其复制内容的进程名
	AutoStart     bool     `json:"autoStart"`     // 开机自启
	PauseCapture  bool     `json:"pauseCapture"`  // 暂停捕获
	PastePlain    bool     `json:"pastePlain"`    // 默认以纯文本粘贴
	PanelHeight   int      `json:"panelHeight"`   // 面板高度(像素)
}

// DefaultSettings 返回出厂配置。
func DefaultSettings() Settings {
	return Settings{
		Hotkey:       "Ctrl+Shift+V",
		MaxItems:     2000,
		ExcludedApps: []string{},
		AutoStart:    false,
		PauseCapture: false,
		PastePlain:   false,
		PanelHeight:  280,
	}
}
