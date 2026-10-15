// Package search 把条目内容预计算成可直接用 LIKE 匹配的检索文本。
//
// 为什么不是全文索引:见 DESIGN.md 11.2。要点是 LIKE 本来就是子串匹配,
// 所以拼音只要**连写**存进去,任何一段都能作为子串命中 ——
// 查询侧不需要任何转换,也就没有查询语法要维护。
package search

import (
	"strings"
	"unicode"

	"github.com/mozillazg/go-pinyin"
)

// translitLimit 是参与拼音转换的最大字符数。
//
// 剪贴板里偶尔会出现几 MB 的文本,而查字典是逐字符的,全量转换会让入库
// 明显变慢。超出部分仍然保留原文(可被普通子串搜到),只是不再有拼音索引。
const translitLimit = 4000

// Build 生成一条条目的检索文本:原文 + 拼音,全部小写。
//
// 传入的多个片段(正文、OCR 文本、来源应用)会被拼接后再转换。
func Build(parts ...string) string {
	var raw strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		raw.WriteString(p)
		raw.WriteByte('\n')
	}

	s := raw.String()
	if s == "" {
		return ""
	}
	// 原文必须**原样**保留:普通子串搜索(含中文)靠的就是它
	return strings.ToLower(s + "\n" + translit(s))
}

// translit 输出"全拼连写 + 首字母连写",非汉字只作为分段边界、不进入结果。
//
// 原文不在本函数的输出里 —— Build 已经把原文整段放进检索文本了。
//
// 逐字调用 SinglePinyin 而不是整串调用 Pinyin:后者会**静默丢弃**
// 非汉字字符,返回的分组数与字符数并不相等,按下标对齐必然错位
// (实测「循环」会被取成 `huan h`)。逐字查询没有这个对齐问题。
func translit(s string) string {
	runes := []rune(s)
	if len(runes) > translitLimit {
		runes = runes[:translitLimit]
	}

	args := pinyin.NewArgs()
	var out, full, initials strings.Builder

	// 一段连续中文连成一个串,段与段之间用空格隔开。
	// 连写是刻意的:搜「xunhuan」要能命中 `xiaoxixunhuanjiance` 的中间一段。
	flush := func() {
		if full.Len() == 0 {
			return
		}
		if out.Len() > 0 {
			out.WriteByte(' ')
		}
		out.WriteString(full.String())
		out.WriteByte(' ')
		out.WriteString(initials.String())
		full.Reset()
		initials.Reset()
	}

	for _, r := range runes {
		if !unicode.Is(unicode.Han, r) {
			flush()
			continue
		}
		syls := pinyin.SinglePinyin(r, args)
		if len(syls) == 0 || syls[0] == "" {
			// 字典里没有这个字,跳过而不是把汉字混进拼音串
			flush()
			continue
		}
		full.WriteString(syls[0])
		initials.WriteByte(syls[0][0])
	}
	flush()

	return out.String()
}
