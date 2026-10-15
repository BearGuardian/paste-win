// Package ocr 调用 Windows 自带的 OCR 引擎,识别图片里的文字。
//
// 引擎本体(Windows.Media.Ocr)是 WinRT API,从 Go 直接调需要手写
// COM/WinRT 的 vtable 互操作;而 Tesseract 一类需要 cgo,与本项目
// CGO_ENABLED=0 的约束冲突。因此绕道 PowerShell —— 它自带 WinRT 投影。
// 详见 DESIGN.md 11.3。
package ocr

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"image"
	"image/png"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"

	"golang.org/x/image/draw"
)

//go:embed ocr.ps1
var script []byte

const (
	// recognizeTimeout 是单张图片的识别上限。实测约 2 秒,留足余量。
	recognizeTimeout = 30 * time.Second

	scriptName = "ocr.ps1"

	// 脚本约定的退出码
	exitNoEngine  = 2
	exitBadImage  = 3
)

// ErrNoEngine 表示本机没有可用的 OCR 引擎。
var ErrNoEngine = errors.New("本机没有可用的 OCR 引擎")

// PipelineVersion 标识当前的识别流水线。
//
// 改动预处理(比如加入放大)后必须递增:启动时会据此把已识别过的图片
// 重新标记为待识别。不这样做的话,老图片会永远停在旧流水线的识别质量上。
const PipelineVersion = "2"

const (
	// targetLongSide 是放大后期望的长边像素数。
	//
	// Windows OCR 对字高约 40px 的文本最准,而聊天窗口截图这类小字远低于此,
	// 引擎会把汉字拆成部件来认 —— 实测「陈」被拆成「阝东」、「诗」拆成「讠寺」。
	// 放大到长边约 2500px 后,「陈」以及正文里的「文档」「渲染」「本地」
	// 都能正确识别。
	targetLongSide = 2500

	// maxScale 限制放大倍数。实测 8 倍反而不如 4 倍 ——
	// 过度插值会把笔画糊在一起,反而更难切分。
	maxScale = 4
)

// Engine 调用系统 OCR。零值不可用,须经 New 构造。
type Engine struct {
	scriptPath string
}

// New 把内嵌的脚本释放到 dir 下,并返回引擎。
//
// 脚本必须落到磁盘:PowerShell 的 -File 只接受文件路径。
// 每次启动都重写一遍,保证磁盘上的脚本与当前程序版本一致。
func New(dir string) (*Engine, error) {
	path := filepath.Join(dir, scriptName)
	if err := os.WriteFile(path, script, 0o644); err != nil {
		return nil, fmt.Errorf("释放 OCR 脚本失败: %w", err)
	}
	return &Engine{scriptPath: path}, nil
}

// Recognize 识别图片中的文字。识别不出内容时返回空串与 nil。
//
// imagePath 必须是**反斜杠**的绝对路径:WinRT 的
// StorageFile.GetFileFromPathAsync 不接受正斜杠,而它给出的报错
// ("发生一个或多个错误")完全不指向真正的原因。
func (e *Engine) Recognize(ctx context.Context, imagePath string) (string, error) {
	path, cleanup, err := e.prepare(imagePath)
	if err != nil {
		return "", err
	}
	defer cleanup()

	ctx, cancel := context.WithTimeout(ctx, recognizeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "powershell.exe",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy", "Bypass",
		"-File", e.scriptPath,
		"-Path", path,
	)
	// 托盘应用不能因为跑个 OCR 就闪一个黑框出来
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			switch ee.ExitCode() {
			case exitNoEngine:
				return "", ErrNoEngine
			case exitBadImage:
				return "", fmt.Errorf("读取图片失败: %s", strings.TrimSpace(stderr.String()))
			}
		}
		if ctx.Err() != nil {
			return "", fmt.Errorf("OCR 超时(%v)", recognizeTimeout)
		}
		return "", fmt.Errorf("OCR 进程失败: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}

	// 脚本已把输出编码设为 UTF-8,这里直接按字符串收
	return normalize(stdout.String()), nil
}

// prepare 在必要时把图片放大,返回实际交给引擎的路径与清理函数。
//
// 只放大、不缩小:已经足够大的图直接原样送进去。
func (e *Engine) prepare(src string) (string, func(), error) {
	noop := func() {}

	f, err := os.Open(src)
	if err != nil {
		return "", noop, err
	}
	img, err := png.Decode(f)
	f.Close()
	if err != nil {
		return "", noop, fmt.Errorf("解码图片失败: %w", err)
	}

	b := img.Bounds()
	long := b.Dx()
	if b.Dy() > long {
		long = b.Dy()
	}
	if long <= 0 {
		return "", noop, fmt.Errorf("图片尺寸非法: %dx%d", b.Dx(), b.Dy())
	}

	scale := int(math.Round(float64(targetLongSide) / float64(long)))
	if scale < 1 {
		scale = 1
	}
	if scale > maxScale {
		scale = maxScale
	}
	if scale == 1 {
		return filepath.FromSlash(src), noop, nil
	}

	dst := image.NewRGBA(image.Rect(0, 0, b.Dx()*scale, b.Dy()*scale))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)

	tmp, err := os.CreateTemp("", "pastewin-ocr-*.png")
	if err != nil {
		return "", noop, fmt.Errorf("创建临时文件失败: %w", err)
	}
	path := tmp.Name()
	if err := png.Encode(tmp, dst); err != nil {
		tmp.Close()
		os.Remove(path)
		return "", noop, fmt.Errorf("写出放大图失败: %w", err)
	}
	tmp.Close()

	log.Printf("OCR: 放大 %d 倍(%d×%d → %d×%d)",
		scale, b.Dx(), b.Dy(), b.Dx()*scale, b.Dy()*scale)
	return path, func() { os.Remove(path) }, nil
}

// normalize 去掉 OCR 在汉字之间插入的空格,并统一换行。
//
// WinRT OCR 的中文输出是「冲 突 性 质」这样**逐字带空格**的。
// 直接入库会让搜「冲突性质」这种连续词命中不到 —— 而逐字搜索不是
// 用户的行为。这里只去掉"前后都是汉字"的空格,英文单词之间的空格必须保留。
func normalize(s string) string {
	runes := []rune(strings.TrimSpace(s))

	var out []rune
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == ' ' || r == '\t' {
			// 向后跳过连续空白,看下一个非空白字符是不是汉字
			j := i
			for j < len(runes) && (runes[j] == ' ' || runes[j] == '\t') {
				j++
			}
			prevHan := len(out) > 0 && unicode.Is(unicode.Han, out[len(out)-1])
			nextHan := j < len(runes) && unicode.Is(unicode.Han, runes[j])
			if prevHan && nextHan {
				i = j - 1 // 整段空白都吞掉
				continue
			}
		}
		out = append(out, r)
	}

	// 统一换行,避免把 \r 带进检索文本
	return strings.ReplaceAll(string(out), "\r\n", "\n")
}
