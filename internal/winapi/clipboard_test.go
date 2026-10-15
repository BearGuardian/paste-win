package winapi

import (
	"encoding/binary"
	"image"
	"image/color"
	"testing"
	"unicode/utf16"
	"unsafe"
)

// utf16Bytes 把字符串编成 CF_UNICODETEXT 所需的 UTF-16LE + NUL 字节序列。
func utf16Bytes(s string) []byte {
	u := utf16.Encode([]rune(s))
	u = append(u, 0)
	b := make([]byte, len(u)*2)
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[i*2:], c)
	}
	return b
}

// withClipboard 打开剪贴板执行 fn,并保证关闭。
func withClipboard(t *testing.T, fn func()) {
	t.Helper()
	if err := OpenClipboardRetry(0, 5); err != nil {
		t.Fatalf("打开剪贴板失败: %v", err)
	}
	defer CloseClipboard()
	fn()
}

// saveClipboardText 备份当前剪贴板文本,测试结束后还原,避免污染用户剪贴板。
func saveClipboardText(t *testing.T) func() {
	t.Helper()
	var saved string
	var had bool
	withClipboard(t, func() {
		if IsFormatAvailable(CF_UNICODETEXT) {
			if s, err := ReadText(); err == nil {
				saved, had = s, true
			}
		}
	})
	return func() {
		if !had {
			return
		}
		withClipboard(t, func() {
			if err := EmptyClipboard(); err != nil {
				t.Logf("还原剪贴板时清空失败: %v", err)
				return
			}
			if err := SetClipboardBytes(CF_UNICODETEXT, utf16Bytes(saved)); err != nil {
				t.Logf("还原剪贴板失败: %v", err)
			}
		})
	}
}

func TestClipboardTextRoundTrip(t *testing.T) {
	defer saveClipboardText(t)()

	const want = "PasteWin 剪贴板往返测试 ✓ — 中文/emoji/符号"

	withClipboard(t, func() {
		if err := EmptyClipboard(); err != nil {
			t.Fatalf("EmptyClipboard 失败: %v", err)
		}
		if err := SetClipboardBytes(CF_UNICODETEXT, utf16Bytes(want)); err != nil {
			t.Fatalf("写入剪贴板失败: %v", err)
		}
	})

	// 重新打开再读,确认数据在关闭剪贴板后依然可读(即内存所有权已正确移交)
	withClipboard(t, func() {
		got, err := ReadText()
		if err != nil {
			t.Fatalf("读取剪贴板失败: %v", err)
		}
		if got != want {
			t.Errorf("往返不一致:\n  写入 %q\n  读出 %q", want, got)
		}
	})
}

func TestClipboardSequenceNumberAdvances(t *testing.T) {
	defer saveClipboardText(t)()

	before := ClipboardSequenceNumber()
	withClipboard(t, func() {
		if err := EmptyClipboard(); err != nil {
			t.Fatalf("EmptyClipboard 失败: %v", err)
		}
		if err := SetClipboardBytes(CF_UNICODETEXT, utf16Bytes("seq-test")); err != nil {
			t.Fatalf("写入剪贴板失败: %v", err)
		}
	})
	after := ClipboardSequenceNumber()

	if after == before {
		t.Errorf("写入后剪贴板序号未变化(%d),去重逻辑会误判为重复内容", before)
	}
}

func TestOpenClipboardRetrySucceeds(t *testing.T) {
	if err := OpenClipboardRetry(0, 5); err != nil {
		t.Fatalf("OpenClipboardRetry 失败: %v", err)
	}
	CloseClipboard()
}

func TestDIBRoundTrip(t *testing.T) {
	// 造一张带透明度的非方形图,顺便覆盖行对齐(宽 3 像素 → 24 位下需补 3 字节)
	const w, h = 3, 2
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	colors := []color.RGBA{
		{R: 255, A: 255}, {G: 255, A: 255}, {B: 255, A: 255},
		{R: 255, G: 255, A: 255}, {R: 255, G: 255, B: 255, A: 255}, {A: 255},
	}
	for i, c := range colors {
		src.SetRGBA(i%w, i/w, c)
	}

	dib := EncodeDIB(src)
	got, err := DecodeDIB(dib)
	if err != nil {
		t.Fatalf("DecodeDIB 失败: %v", err)
	}
	if b := got.Bounds(); b.Dx() != w || b.Dy() != h {
		t.Fatalf("尺寸不符: 得到 %dx%d,期望 %dx%d", b.Dx(), b.Dy(), w, h)
	}
	for y := range h {
		for x := range w {
			wantR, wantG, wantB, _ := src.At(x, y).RGBA()
			gotR, gotG, gotB, _ := got.At(x, y).RGBA()
			if wantR != gotR || wantG != gotG || wantB != gotB {
				t.Errorf("像素 (%d,%d) 不符: 得到 (%d,%d,%d),期望 (%d,%d,%d)",
					x, y, gotR>>8, gotG>>8, gotB>>8, wantR>>8, wantG>>8, wantB>>8)
			}
		}
	}
}

func TestDecodeDIBRejectsGarbage(t *testing.T) {
	if _, err := DecodeDIB([]byte{1, 2, 3}); err == nil {
		t.Error("过短的输入应当报错,却返回了成功")
	}
	// 头合法但尺寸为 0
	hdr := make([]byte, 40)
	binary.LittleEndian.PutUint32(hdr[biSize:], 40)
	binary.LittleEndian.PutUint16(hdr[biPlanes:], 1)
	binary.LittleEndian.PutUint16(hdr[biBitCount:], 24)
	if _, err := DecodeDIB(hdr); err == nil {
		t.Error("尺寸为 0 的 DIB 应当报错,却返回了成功")
	}
}

// TestInputStructLayout 锁定 INPUT 结构的大小。
//
// Win32 的 INPUT 里 union 由最大成员 MOUSEINPUT 决定,为 32 字节,
// 故 sizeof(INPUT) = 8 + 32 = 40。少补字节会让 SendInput 返回
// ERROR_INVALID_PARAMETER,而且失败是静默的(按键就是发不出去),
// 很难排查,所以在这里钉死。
func TestInputStructLayout(t *testing.T) {
	const wantInputSize = 40
	const wantKeybdSize = 24

	if got := unsafe.Sizeof(keybdInput{}); got != wantKeybdSize {
		t.Errorf("KEYBDINPUT 大小应为 %d,实际 %d", wantKeybdSize, got)
	}
	if got := unsafe.Sizeof(input{}); got != wantInputSize {
		t.Errorf("INPUT 大小应为 %d(含 MOUSEINPUT 的 32 字节 union),实际 %d",
			wantInputSize, got)
	}
}
