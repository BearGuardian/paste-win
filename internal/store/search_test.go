package store

import (
	"testing"

	"pastewin/internal/model"
)

// keywordList 按关键字查询,返回命中的条目。
func keywordList(t *testing.T, s *Store, kw string) []model.Item {
	t.Helper()
	items, err := s.List(model.Filter{Keyword: kw})
	if err != nil {
		t.Fatalf("按 %q 查询失败: %v", kw, err)
	}
	return items
}

func TestSearchFindsChineseSubstring(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.Insert(textContent("消息循环检测_1792034852"), "", ""); err != nil {
		t.Fatal(err)
	}

	// LIKE 本来就是子串匹配,中文不需要分词
	for _, kw := range []string{"消息", "循环检测", "1792034852"} {
		if got := keywordList(t, s, kw); len(got) != 1 {
			t.Errorf("搜 %q 应命中 1 条,实际 %d 条", kw, len(got))
		}
	}
}

func TestSearchFindsPinyin(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.Insert(textContent("消息循环检测"), "", ""); err != nil {
		t.Fatal(err)
	}

	// 全拼:整串、开头一段、中间一段都要能命中 ——
	// 这正是不按音节切分、直接连写存进去的意义
	for _, kw := range []string{"xiaoxi", "xunhuan", "jiance", "xiaoxixunhuan"} {
		if got := keywordList(t, s, kw); len(got) != 1 {
			t.Errorf("拼音 %q 应命中 1 条,实际 %d 条", kw, len(got))
		}
	}
}

func TestSearchFindsInitials(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.Insert(textContent("消息循环检测"), "", ""); err != nil {
		t.Fatal(err)
	}

	if got := keywordList(t, s, "xxhjc"); len(got) != 1 {
		t.Errorf("首字母 xxhjc 应命中 1 条,实际 %d 条", len(got))
	}
}

func TestSearchIsCaseInsensitive(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.Insert(textContent("HTTPServer"), "", ""); err != nil {
		t.Fatal(err)
	}
	if got := keywordList(t, s, "httpserver"); len(got) != 1 {
		t.Errorf("小写查询应命中,实际 %d 条", len(got))
	}
	if got := keywordList(t, s, "HTTPSERVER"); len(got) != 1 {
		t.Errorf("大写查询应命中,实际 %d 条", len(got))
	}
}

func TestSearchStillEscapesWildcards(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.Insert(textContent("折扣 100% 起"), "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(textContent("完全无关的内容"), "", ""); err != nil {
		t.Fatal(err)
	}

	// 未转义的话 "%" 会变成通配符,把无关条目也匹配进来
	if got := keywordList(t, s, "100%"); len(got) != 1 {
		t.Errorf("含通配符的关键字应命中 1 条,实际 %d 条", len(got))
	}
}

func TestUpdateContentRefreshesSearch(t *testing.T) {
	s := newTestStore(t)
	it, _, err := s.Insert(textContent("原始内容"), "", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.UpdateContent(it.ID, "改成了新内容"); err != nil {
		t.Fatalf("更新内容失败: %v", err)
	}

	if got := keywordList(t, s, "新内容"); len(got) != 1 {
		t.Errorf("更新后应能按新内容搜到,实际 %d 条", len(got))
	}
	if got := keywordList(t, s, "原始内容"); len(got) != 0 {
		t.Errorf("更新后不应再按旧内容搜到,实际 %d 条", len(got))
	}
	// 拼音索引也要跟着刷新
	if got := keywordList(t, s, "xinneirong"); len(got) != 1 {
		t.Errorf("更新后应能按新内容拼音搜到,实际 %d 条", len(got))
	}
}

func TestSetOCRTextMakesImageSearchable(t *testing.T) {
	s := newTestStore(t)
	it, _, err := s.Insert(model.ClipContent{
		Kind:     model.KindImage,
		Preview:  "图片 800 × 600",
		Hash:     "img-1",
		ByteSize: 1234,
	}, "abc.png", "abc_thumb.png")
	if err != nil {
		t.Fatal(err)
	}

	// 识别前:图片内容搜不到
	if got := keywordList(t, s, "登录失败"); len(got) != 0 {
		t.Fatalf("识别前不应命中,实际 %d 条", len(got))
	}

	if err := s.SetOCRText(it.ID, "登录失败,请重试"); err != nil {
		t.Fatalf("写入 OCR 失败: %v", err)
	}

	if got := keywordList(t, s, "登录失败"); len(got) != 1 {
		t.Errorf("识别后应能按图中文字搜到,实际 %d 条", len(got))
	}
	if got := keywordList(t, s, "denglushibai"); len(got) != 1 {
		t.Errorf("识别后应能按图中文字拼音搜到,实际 %d 条", len(got))
	}
}

func TestImagesNeedingOCR(t *testing.T) {
	s := newTestStore(t)

	img, _, err := s.Insert(model.ClipContent{
		Kind: model.KindImage, Hash: "img-a",
	}, "a.png", "a_t.png")
	if err != nil {
		t.Fatal(err)
	}
	// 没有 blob 的图片条目不该进队列(文件都不在,识别无从谈起)
	if _, _, err := s.Insert(model.ClipContent{
		Kind: model.KindImage, Hash: "img-b",
	}, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(textContent("纯文本"), "", ""); err != nil {
		t.Fatal(err)
	}

	pending, err := s.ImagesNeedingOCR(10)
	if err != nil {
		t.Fatalf("查询待识别图片失败: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("应只有 1 条待识别,实际 %d 条", len(pending))
	}
	if pending[0].ID != img.ID || pending[0].Path != "a.png" {
		t.Errorf("待识别条目不对: %+v", pending[0])
	}

	// 识别完成后不应再出现在队列里
	if err := s.SetOCRText(img.ID, "识别结果"); err != nil {
		t.Fatal(err)
	}
	pending, err = s.ImagesNeedingOCR(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("识别后队列应为空,实际 %d 条", len(pending))
	}
}

// 识别不出文字的图片也必须标记为"已尝试",否则每次启动都会重跑一遍
func TestOCRDonePreventsRequeue(t *testing.T) {
	s := newTestStore(t)
	img, _, err := s.Insert(model.ClipContent{
		Kind: model.KindImage, Hash: "img-empty",
	}, "e.png", "e_t.png")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.SetOCRText(img.ID, ""); err != nil {
		t.Fatal(err)
	}

	pending, err := s.ImagesNeedingOCR(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("识别过(即使没文字)的图片不应再入队,实际 %d 条", len(pending))
	}
}
