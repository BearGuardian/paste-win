package search

import (
	"strings"
	"testing"
)

func TestBuildIncludesOriginalAndPinyin(t *testing.T) {
	got := Build("消息循环检测_1792034852")

	// 原文必须原样保留,普通子串搜索(含中文)才成立
	if !strings.Contains(got, "消息循环检测_1792034852") {
		t.Fatalf("检索文本缺少原文: %q", got)
	}
	// 全拼连写:搜中间任意一段都要能命中
	if !strings.Contains(got, "xiaoxixunhuanjiance") {
		t.Errorf("缺少全拼连写: %q", got)
	}
	// 首字母连写:消x 息x 循x 环h 检j 测c
	if !strings.Contains(got, "xxxhjc") {
		t.Errorf("缺少首字母连写: %q", got)
	}
	if got != strings.ToLower(got) {
		t.Errorf("检索文本未全部小写: %q", got)
	}
}

// 拼音连写要能支持"搜中间一段",这是不按音节切分的前提
func TestBuildSupportsMiddleOfPinyinRun(t *testing.T) {
	got := Build("消息循环检测")
	for _, frag := range []string{"xunhuan", "jiance", "xiaoxi"} {
		if !strings.Contains(got, frag) {
			t.Errorf("检索文本 %q 中找不到拼音片段 %q", got, frag)
		}
	}
}

func TestBuildKeepsAsciiSearchable(t *testing.T) {
	got := Build("sk-043b175617a4e38bd6715da3b8f")
	if !strings.Contains(got, "sk-043b175617a4e38bd6715da3b8f") {
		t.Fatalf("ASCII 原文丢失: %q", got)
	}
}

func TestBuildUppercaseLowered(t *testing.T) {
	if got := Build("HTTPServer"); !strings.Contains(got, "httpserver") {
		t.Fatalf("未转小写: %q", got)
	}
}

// 两段中文之间隔着非中文字符时各自连写,不能凭空拼成一串
func TestBuildSeparatesRuns(t *testing.T) {
	got := Build("消息 循环")
	if !strings.Contains(got, "xiaoxi") || !strings.Contains(got, "xunhuan") {
		t.Fatalf("分段拼音缺失: %q", got)
	}
	if strings.Contains(got, "xiaoxixunhuan") {
		t.Errorf("两段中文被拼成了一串: %q", got)
	}
}

func TestBuildEmpty(t *testing.T) {
	if got := Build(""); got != "" {
		t.Errorf("空输入应返回空串,得到 %q", got)
	}
	if got := Build("", ""); got != "" {
		t.Errorf("全空片段应返回空串,得到 %q", got)
	}
}

func TestBuildMergesParts(t *testing.T) {
	got := Build("截图", "登录失败")
	if !strings.Contains(got, "登录失败") {
		t.Errorf("片段原文未合并: %q", got)
	}
	if !strings.Contains(got, "jietu") || !strings.Contains(got, "lushibai") {
		t.Errorf("片段拼音未合并: %q", got)
	}
}

// 非汉字不能混进拼音串,否则会污染首字母匹配
func TestTranslitSkipsNonHan(t *testing.T) {
	got := Build("测试🚀abc")
	if !strings.Contains(got, "ceshi") {
		t.Errorf("正常汉字拼音丢失: %q", got)
	}
	if strings.Contains(got, "ceshi🚀") {
		t.Errorf("非汉字被混进拼音串: %q", got)
	}
	if strings.Contains(got, "cs🚀") {
		t.Errorf("首字母串被非汉字污染: %q", got)
	}
}

// 超过转换上限的部分不再有拼音,但原文仍然完整可搜
func TestBuildLongTextKeepsOriginal(t *testing.T) {
	long := strings.Repeat("测试内容", 2000) // 8000 字,超过 translitLimit
	got := Build(long)
	if !strings.Contains(got, long) {
		t.Error("超长文本的原文未完整保留")
	}
	if !strings.Contains(got, "ceshi") {
		t.Error("超长文本完全没做拼音转换")
	}
}
