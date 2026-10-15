package ocr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("创建引擎失败: %v", err)
	}
	return e
}

func TestNewWritesScript(t *testing.T) {
	dir := t.TempDir()
	e, err := New(dir)
	if err != nil {
		t.Fatalf("创建引擎失败: %v", err)
	}

	data, err := os.ReadFile(e.scriptPath)
	if err != nil {
		t.Fatalf("脚本未落盘: %v", err)
	}
	if !strings.Contains(string(data), "RecognizeAsync") {
		t.Error("落盘的脚本内容不对")
	}
	if filepath.Base(e.scriptPath) != scriptName {
		t.Errorf("脚本文件名不对: %s", e.scriptPath)
	}
}

// 脚本必须是纯 ASCII。PowerShell 5.1 对不带 BOM 的 .ps1 按系统 ANSI
// 代码页读取,本机是 GBK —— 混进非 ASCII 会把语法解析带崩,
// 而且报错行号与实际内容对不上,极难排查。见 DESIGN.md 11.3。
func TestScriptIsASCIIOnly(t *testing.T) {
	for i, b := range script {
		if b > 0x7f {
			t.Fatalf("脚本第 %d 字节为 0x%02x,不是纯 ASCII", i, b)
		}
	}
}

func TestRecognizeMissingFile(t *testing.T) {
	e := newTestEngine(t)

	missing := filepath.Join(t.TempDir(), "does-not-exist.png")
	_, err := e.Recognize(context.Background(), missing)
	if err == nil {
		t.Fatal("对不存在的文件应当报错")
	}
	// 文件在读入阶段就被打开,所以这里不会走到 PowerShell ——
	// 失败得更早、错误也更直白,正是想要的行为
	if !strings.Contains(err.Error(), "does-not-exist.png") {
		t.Errorf("错误信息应带上出问题的路径,实际: %v", err)
	}
}

// 非图片文件应当在解码阶段被拒,而不是把垃圾丢给引擎
func TestRecognizeNonImage(t *testing.T) {
	e := newTestEngine(t)

	junk := filepath.Join(t.TempDir(), "junk.png")
	if err := os.WriteFile(junk, []byte("this is not a png"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := e.Recognize(context.Background(), junk)
	if err == nil {
		t.Fatal("非图片文件应当报错")
	}
	if !strings.Contains(err.Error(), "解码图片失败") {
		t.Errorf("错误信息应指向解码失败,实际: %v", err)
	}
}

// 对真实截图的识别是集成测试:需要一张图片,由环境变量指定。
//
//	PASTEWIN_OCR_TEST_IMAGE=D:\path\to\shot.png go test ./internal/ocr/ -run RealImage -v
func TestRecognizeRealImage(t *testing.T) {
	path := os.Getenv("PASTEWIN_OCR_TEST_IMAGE")
	if path == "" {
		t.Skip("未设置 PASTEWIN_OCR_TEST_IMAGE,跳过真实图片识别")
	}

	e := newTestEngine(t)
	got, err := e.Recognize(context.Background(), path)
	if err != nil {
		t.Fatalf("识别失败: %v", err)
	}
	if strings.TrimSpace(got) == "" {
		t.Fatal("识别结果为空")
	}
	t.Logf("识别结果: %s", got)
}

func TestNormalizeCollapsesSpacesBetweenHan(t *testing.T) {
	cases := []struct{ in, want string }{
		// WinRT OCR 的实际输出形态:逐字带空格
		{"冲 突 性 质", "冲突性质"},
		{"登 录 失 败 , 请 重 试", "登录失败 , 请重试"},
		// 英文单词之间的空格必须保留,否则英文就粘连了
		{"LumiWorkspacePublishService. java", "LumiWorkspacePublishService. java"},
		{"hello world", "hello world"},
		// 中英混排:汉字之间去掉,汉字与英文之间保留
		{"消息 loop 检测", "消息 loop 检测"},
		// 连续空白整体吞掉
		{"冲  突", "冲突"},
		// 换行统一
		{"第一行\r\n第二行", "第一行\n第二行"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalize(c.in); got != c.want {
			t.Errorf("normalize(%q) = %q,期望 %q", c.in, got, c.want)
		}
	}
}
