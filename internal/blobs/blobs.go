// Package blobs 把图片内容落盘为 PNG,并生成缩略图。
//
// 图片不进数据库:SQLite 里塞几 MB 的 BLOB 会让列表查询和备份都变重,
// 而图片本身不需要参与查询。库里只存相对路径。
package blobs

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"

	xdraw "golang.org/x/image/draw"
)

// 缩略图最大边长。卡片在界面上约 100px 高,2 倍图足够清晰。
const thumbMaxEdge = 256

// Store 管理 blob 目录。
type Store struct {
	baseDir string // 数据目录的绝对路径
	blobDir string // baseDir/blobs
}

// New 创建(必要时初始化)blob 目录。
func New(baseDir string) (*Store, error) {
	dir := filepath.Join(baseDir, "blobs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建图片目录失败: %w", err)
	}
	return &Store{baseDir: baseDir, blobDir: dir}, nil
}

// SaveImage 把 PNG 数据写入磁盘,返回相对数据目录的路径。
//
// 文件名用条目 ID,天然唯一且便于按 ID 反查。
func (s *Store) SaveImage(id string, pngData []byte) (string, error) {
	rel := filepath.Join("blobs", id+".png")
	if err := os.WriteFile(filepath.Join(s.baseDir, rel), pngData, 0o644); err != nil {
		return "", fmt.Errorf("写入图片失败: %w", err)
	}
	return rel, nil
}

// MakeThumb 由 PNG 数据生成缩略图并落盘,返回相对路径。
//
// 原图小于阈值时直接复用原图,不产生多余的放大或重编码。
func (s *Store) MakeThumb(id string, pngData []byte) (string, error) {
	src, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		return "", fmt.Errorf("解码 PNG 失败: %w", err)
	}

	b := src.Bounds()
	if b.Dx() <= thumbMaxEdge && b.Dy() <= thumbMaxEdge {
		return s.SaveImage(id+"_thumb", pngData)
	}

	dst := fitWithin(b.Dx(), b.Dy(), thumbMaxEdge)
	// CatmullRom 比最近邻慢一点,但缩略图最怕锯齿,这个代价值得
	scaled := image.NewRGBA(image.Rect(0, 0, dst.x, dst.y))
	xdraw.CatmullRom.Scale(scaled, scaled.Bounds(), src, b, xdraw.Over, nil)

	var buf bytes.Buffer
	if err := png.Encode(&buf, scaled); err != nil {
		return "", fmt.Errorf("编码缩略图失败: %w", err)
	}
	return s.SaveImage(id+"_thumb", buf.Bytes())
}

type size struct{ x, y int }

// fitWithin 按比例缩放到不超过 maxEdge,至少 1 像素。
func fitWithin(w, h, maxEdge int) size {
	if w <= 0 || h <= 0 {
		return size{1, 1}
	}
	if w >= h {
		nw := maxEdge
		nh := h * maxEdge / w
		return size{nw, max(1, nh)}
	}
	nh := maxEdge
	nw := w * maxEdge / h
	return size{max(1, nw), nh}
}

// Remove 删除相对路径指向的文件。文件已不存在不算错误 ——
// 清理流程会重复调用,不该因为缺文件而中断。
func (s *Store) Remove(rel string) error {
	if rel == "" {
		return nil
	}
	abs, err := s.resolve(rel)
	if err != nil {
		return err
	}
	if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除图片失败: %w", err)
	}
	return nil
}

// RemoveAll 批量删除,忽略单个失败并汇总错误。
func (s *Store) RemoveAll(rels []string) error {
	var errs []string
	for _, rel := range rels {
		if err := s.Remove(rel); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("部分图片删除失败: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Abs 把相对路径还原成绝对路径。
func (s *Store) Abs(rel string) (string, error) { return s.resolve(rel) }

// resolve 拼绝对路径,并确保结果仍在数据目录内。
//
// 相对路径来自数据库,而数据库理论上可能被外部篡改;不校验的话
// 一个 "../../" 就能让删除操作跑到数据目录之外。
func (s *Store) resolve(rel string) (string, error) {
	abs := filepath.Join(s.baseDir, rel)

	baseClean := filepath.Clean(s.baseDir) + string(os.PathSeparator)
	if !strings.HasPrefix(filepath.Clean(abs)+string(os.PathSeparator), baseClean) {
		return "", fmt.Errorf("路径越出数据目录: %s", rel)
	}
	return abs, nil
}

// LoadPNG 读回原图,用于回贴图片到剪贴板。
func (s *Store) LoadPNG(rel string) ([]byte, error) {
	abs, err := s.resolve(rel)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("读取图片失败: %w", err)
	}
	return data, nil
}
