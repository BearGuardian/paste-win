package winapi

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
)

// BITMAPINFOHEADER / BITMAPV5HEADER 的字段偏移
const (
	biSize        = 0
	biWidth       = 4
	biHeight      = 8
	biPlanes      = 12
	biBitCount    = 14
	biCompression = 16
	biClrUsed     = 32

	// BITMAPV5HEADER 独有:三个颜色掩码,共 124 字节头
	bV5RedMask   = 40
	bV5GreenMask = 44
	bV5BlueMask  = 48
	bV5AlphaMask = 52

	headerSizeV5 = 124
)

// 压缩方式
const (
	biRGB       = 0
	biBitFields = 3
)

// DecodeDIB 把 CF_DIB / CF_DIBV5 的原始字节解成 image.Image。
//
// CF_DIB 数据 = BITMAPINFOHEADER(或 V5)+ 可选调色板 + 像素数据。
// 像素行按 4 字节对齐,且 biHeight 为正时是自下而上存储 —— 两个坑都得处理。
// 这里覆盖截图场景实际会遇到的格式:24/32 位无压缩,以及 32 位带掩码。
func DecodeDIB(data []byte) (image.Image, error) {
	if len(data) < 40 {
		return nil, fmt.Errorf("DIB 数据过短: %d 字节", len(data))
	}

	hdrSize := binary.LittleEndian.Uint32(data[biSize:])
	width := int(int32(binary.LittleEndian.Uint32(data[biWidth:])))
	height := int(int32(binary.LittleEndian.Uint32(data[biHeight:])))
	bitCount := int(binary.LittleEndian.Uint16(data[biBitCount:]))
	compression := binary.LittleEndian.Uint32(data[biCompression:])
	clrUsed := int(binary.LittleEndian.Uint32(data[biClrUsed:]))

	if width <= 0 || height == 0 {
		return nil, fmt.Errorf("DIB 尺寸非法: %dx%d", width, height)
	}

	// biHeight 为负表示自上而下存储
	topDown := height < 0
	if topDown {
		height = -height
	}

	switch {
	case bitCount == 24 && compression == biRGB:
		return decode24(data, hdrSize, clrUsed, width, height, topDown)
	case bitCount == 32 && (compression == biRGB || compression == biBitFields):
		return decode32(data, hdrSize, clrUsed, width, height, topDown, compression)
	default:
		return nil, fmt.Errorf("暂不支持的 DIB 格式: %d 位,压缩方式 %d", bitCount, compression)
	}
}

// pixelDataOffset 计算像素数据起点,跳过头部与调色板。
func pixelDataOffset(hdrSize uint32, clrUsed, bitCount int) int {
	off := int(hdrSize)
	if bitCount <= 8 {
		n := clrUsed
		if n == 0 {
			n = 1 << bitCount // 未指定时用满调色板
		}
		off += n * 4 // RGBQUAD
	}
	return off
}

// rowStride 返回每行字节数(4 字节对齐)。
func rowStride(width, bitCount int) int {
	return ((width*bitCount + 31) / 32) * 4
}

func decode24(data []byte, hdrSize uint32, clrUsed, width, height int, topDown bool) (image.Image, error) {
	off := pixelDataOffset(hdrSize, clrUsed, 24)
	stride := rowStride(width, 24)
	if off+stride*height > len(data) {
		return nil, fmt.Errorf("DIB 像素数据不完整: 需要 %d,实际 %d", off+stride*height, len(data))
	}

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		srcY := y
		if !topDown {
			srcY = height - 1 - y // 自下而上
		}
		row := data[off+srcY*stride:]
		for x := range width {
			b := row[x*3+0]
			g := row[x*3+1]
			r := row[x*3+2]
			img.SetRGBA(x, y, color.RGBA{R: r, G: g, B: b, A: 255})
		}
	}
	return img, nil
}

func decode32(data []byte, hdrSize uint32, clrUsed, width, height int, topDown bool, compression uint32) (image.Image, error) {
	off := pixelDataOffset(hdrSize, clrUsed, 32)
	stride := rowStride(width, 32)
	if off+stride*height > len(data) {
		return nil, fmt.Errorf("DIB 像素数据不完整: 需要 %d,实际 %d", off+stride*height, len(data))
	}

	// BI_RGB 的 32 位图 alpha 通道通常为 0(表示不透明),不能直接当透明度用;
	// 只有 BI_BITFIELDS / V5 头里的 alpha 掩码才可信。
	hasAlpha := compression == biBitFields && hdrSize >= headerSizeV5 &&
		binary.LittleEndian.Uint32(data[bV5AlphaMask:]) != 0

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		srcY := y
		if !topDown {
			srcY = height - 1 - y
		}
		row := data[off+srcY*stride:]
		for x := range width {
			b := row[x*4+0]
			g := row[x*4+1]
			r := row[x*4+2]
			a := row[x*4+3]
			if !hasAlpha {
				a = 255
			}
			img.SetRGBA(x, y, color.RGBA{R: r, G: g, B: b, A: a})
		}
	}
	return img, nil
}

// EncodeDIB 把 image.Image 编成 CF_DIB(32 位 BI_RGB,自下而上)。
// 回贴图片时写回剪贴板用。
func EncodeDIB(img image.Image) []byte {
	b := img.Bounds()
	width, height := b.Dx(), b.Dy()
	stride := rowStride(width, 32)

	out := make([]byte, 40+stride*height)

	// BITMAPINFOHEADER
	binary.LittleEndian.PutUint32(out[biSize:], 40)
	binary.LittleEndian.PutUint32(out[biWidth:], uint32(int32(width)))
	binary.LittleEndian.PutUint32(out[biHeight:], uint32(int32(height))) // 正数 = 自下而上
	binary.LittleEndian.PutUint16(out[biPlanes:], 1)
	binary.LittleEndian.PutUint16(out[biBitCount:], 32)
	binary.LittleEndian.PutUint32(out[biCompression:], biRGB)
	binary.LittleEndian.PutUint32(out[biSizeImageOffset:], uint32(stride*height))

	pix := out[40:]
	for y := range height {
		srcY := b.Min.Y + (height - 1 - y) // 翻转成自下而上
		row := pix[y*stride:]
		for x := range width {
			r, g, bl, a := img.At(b.Min.X+x, srcY).RGBA()
			row[x*4+0] = byte(bl >> 8)
			row[x*4+1] = byte(g >> 8)
			row[x*4+2] = byte(r >> 8)
			row[x*4+3] = byte(a >> 8)
		}
	}
	return out
}

// biSizeImage 字段在头部内的偏移
const biSizeImageOffset = 20
