package store

import (
	"testing"

	"pastewin/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func textContent(s string) model.ClipContent {
	return model.ClipContent{
		Kind:      model.KindText,
		Content:   s,
		PlainText: s,
		Preview:   s,
		Hash:      "hash-" + s,
		ByteSize:  int64(len(s)),
	}
}

func TestInsertAndGet(t *testing.T) {
	s := newTestStore(t)

	it, isNew, err := s.Insert(textContent("hello"), "", "")
	if err != nil {
		t.Fatalf("插入失败: %v", err)
	}
	if !isNew {
		t.Error("首次插入应标记为新建")
	}
	if it.ID == "" {
		t.Error("插入后应分配 ID")
	}

	got, err := s.Get(it.ID)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.Content != "hello" || got.Kind != model.KindText {
		t.Errorf("内容不符: %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Error("时间戳未设置")
	}
}

func TestInsertDeduplicatesSameHash(t *testing.T) {
	s := newTestStore(t)

	first, _, err := s.Insert(textContent("same"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	// 中间插一条别的,确保顺序可观测
	if _, _, err := s.Insert(textContent("other"), "", ""); err != nil {
		t.Fatal(err)
	}
	second, isNew, err := s.Insert(textContent("same"), "", "")
	if err != nil {
		t.Fatal(err)
	}

	if isNew {
		t.Error("相同 hash 不应新建条目")
	}
	if second.ID != first.ID {
		t.Errorf("应复用同一条目: 得到 %s,期望 %s", second.ID, first.ID)
	}

	n, err := s.Count()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("去重后应只有 2 条,实际 %d", n)
	}

	// 重复插入应把它提到最前
	items, err := s.List(model.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 || items[0].ID != first.ID {
		t.Errorf("重复内容应被提到列表最前,实际顺序: %+v", idsOf(items))
	}
}

func idsOf(items []model.Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Content
	}
	return out
}

func TestListFilterByKeywordAndKind(t *testing.T) {
	s := newTestStore(t)

	seed := []model.ClipContent{
		{Kind: model.KindText, Content: "alpha 苹果", PlainText: "alpha 苹果", Preview: "alpha 苹果", Hash: "h1", SourceApp: "chrome"},
		{Kind: model.KindText, Content: "beta 香蕉", PlainText: "beta 香蕉", Preview: "beta 香蕉", Hash: "h2", SourceApp: "notepad"},
		{Kind: model.KindImage, Content: "", PlainText: "", Preview: "图片 10 × 10", Hash: "h3", SourceApp: "chrome"},
	}
	for _, c := range seed {
		if _, _, err := s.Insert(c, "", ""); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("按关键字", func(t *testing.T) {
		items, err := s.List(model.Filter{Keyword: "苹果"})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0].Hash != "h1" {
			t.Errorf("关键字过滤结果不符: %v", idsOf(items))
		}
	})

	t.Run("按类型", func(t *testing.T) {
		items, err := s.List(model.Filter{Kind: model.KindImage})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0].Hash != "h3" {
			t.Errorf("类型过滤结果不符: %v", idsOf(items))
		}
	})

	t.Run("按来源应用", func(t *testing.T) {
		items, err := s.List(model.Filter{SourceApp: "chrome"})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 2 {
			t.Errorf("来源过滤应命中 2 条,实际 %d", len(items))
		}
	})

	t.Run("LIKE 通配符被转义", func(t *testing.T) {
		if _, _, err := s.Insert(textContent("折扣 100%"), "", ""); err != nil {
			t.Fatal(err)
		}
		// 未转义时 "%" 会变成通配符,匹配到所有条目
		items, err := s.List(model.Filter{Keyword: "100%"})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 {
			t.Errorf(`搜索 "100%%" 应只命中 1 条,实际 %d —— LIKE 通配符未转义`, len(items))
		}
	})
}

func TestPinnedItemsSortFirst(t *testing.T) {
	s := newTestStore(t)

	old, _, err := s.Insert(textContent("old"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(textContent("new"), "", ""); err != nil {
		t.Fatal(err)
	}

	// 未置顶时最新的在最前
	items, _ := s.List(model.Filter{})
	if items[0].Content != "new" {
		t.Fatalf("排序异常: %v", idsOf(items))
	}

	pinned, err := s.TogglePin(old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !pinned {
		t.Error("TogglePin 应返回 true")
	}

	items, _ = s.List(model.Filter{})
	if items[0].ID != old.ID {
		t.Errorf("置顶条目应排最前,实际: %v", idsOf(items))
	}
}

func TestPruneKeepsPinnedAndPinboarded(t *testing.T) {
	s := newTestStore(t)

	board, err := s.CreatePinboard("重要")
	if err != nil {
		t.Fatal(err)
	}

	// 造 10 条
	var ids []string
	for i := range 10 {
		it, _, err := s.Insert(textContent(string(rune('a'+i))), "", "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, it.ID)
	}

	// 第 1 条置顶,第 2 条入分组 —— 两者都不该被清理
	if _, err := s.TogglePin(ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveToPinboard(ids[1], board.ID); err != nil {
		t.Fatal(err)
	}

	// maxItems 约束的是"未受保护"的条目数:10 条里有 2 条受保护,
	// 剩下 8 条要裁到 3 条,因此删掉最旧的 5 条,最终剩 3 + 2 = 5 条。
	res, err := s.Prune(3)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if res.Count != 5 {
		t.Errorf("应清理 5 条,实际 %d", res.Count)
	}

	remaining, _ := s.List(model.Filter{Limit: 100})
	if len(remaining) != 5 {
		t.Fatalf("清理后应剩 5 条,实际 %d: %v", len(remaining), idsOf(remaining))
	}

	// 受保护的必须还在
	kept := map[string]bool{}
	for _, it := range remaining {
		kept[it.ID] = true
	}
	if !kept[ids[0]] {
		t.Error("置顶条目被误删")
	}
	if !kept[ids[1]] {
		t.Error("分组内条目被误删")
	}

	// 保留的应是较新的未受保护条目(ids 按插入顺序,越靠后越新)
	for _, want := range ids[7:] {
		if !kept[want] {
			t.Errorf("较新的条目被误删,应保留最新的未受保护条目")
		}
	}
	// 最旧的未受保护条目应被清掉
	if kept[ids[2]] {
		t.Error("最旧的未受保护条目应被清理")
	}
}

func TestDeleteReturnsBlobPaths(t *testing.T) {
	s := newTestStore(t)

	it, _, err := s.Insert(model.ClipContent{
		Kind: model.KindImage, Hash: "img", Preview: "图片",
	}, "blobs/a.png", "blobs/a_thumb.png")
	if err != nil {
		t.Fatal(err)
	}

	blob, thumb, err := s.Delete(it.ID)
	if err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if blob != "blobs/a.png" || thumb != "blobs/a_thumb.png" {
		t.Errorf("应返回 blob 路径以便清理磁盘: %q / %q", blob, thumb)
	}
	if _, err := s.Get(it.ID); err == nil {
		t.Error("删除后仍能读到条目")
	}
}

func TestPinboardCRUD(t *testing.T) {
	s := newTestStore(t)

	b, err := s.CreatePinboard("代码片段")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePinboard("代码片段"); err == nil {
		t.Error("重名分组应报错")
	}
	if _, err := s.CreatePinboard("   "); err == nil {
		t.Error("空名分组应报错")
	}

	it, _, err := s.Insert(textContent("snippet"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MoveToPinboard(it.ID, b.ID); err != nil {
		t.Fatal(err)
	}

	boards, err := s.ListPinboards()
	if err != nil {
		t.Fatal(err)
	}
	if len(boards) != 1 || boards[0].ItemCount != 1 {
		t.Fatalf("分组条目数统计错误: %+v", boards)
	}

	// 删除分组后条目本身应保留,只是解除归属
	if err := s.DeletePinboard(b.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(it.ID)
	if err != nil {
		t.Fatalf("删除分组不应连带删除条目: %v", err)
	}
	if got.PinboardID != "" {
		t.Errorf("条目应已解除分组归属,实际仍为 %q", got.PinboardID)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	s := newTestStore(t)

	// 未写入时应拿到默认值
	def, err := s.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if def.Hotkey != "Ctrl+Shift+V" || def.MaxItems != 2000 {
		t.Errorf("默认配置不符: %+v", def)
	}

	want := def
	want.Hotkey = "Alt+V"
	want.MaxItems = 500
	want.ExcludedApps = []string{"keepass", "1password"}
	want.AutoStart = true
	if err := s.SaveSettings(want); err != nil {
		t.Fatal(err)
	}

	got, err := s.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if got.Hotkey != "Alt+V" || got.MaxItems != 500 || !got.AutoStart {
		t.Errorf("配置往返不一致: %+v", got)
	}
	if len(got.ExcludedApps) != 2 || got.ExcludedApps[0] != "keepass" {
		t.Errorf("排除名单往返不一致: %v", got.ExcludedApps)
	}
}

func TestSettingsNormalizesBadValues(t *testing.T) {
	s := newTestStore(t)

	if err := s.SaveSettings(model.Settings{Hotkey: "", MaxItems: -5, PanelHeight: 10}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if got.Hotkey == "" {
		t.Error("空热键应被回退为默认值")
	}
	if got.MaxItems <= 0 {
		t.Errorf("非法上限应被修正,实际 %d", got.MaxItems)
	}
	if got.PanelHeight < 120 {
		t.Errorf("非法面板高度应被修正,实际 %d", got.PanelHeight)
	}
}

func TestUpdateContent(t *testing.T) {
	s := newTestStore(t)

	it, _, err := s.Insert(textContent("原始内容"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateContent(it.ID, "编辑后的内容"); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	got, _ := s.Get(it.ID)
	if got.Content != "编辑后的内容" || got.Preview != "编辑后的内容" {
		t.Errorf("内容与摘要都应更新: %+v", got)
	}

	if err := s.UpdateContent(it.ID, "   "); err == nil {
		t.Error("空内容应被拒绝")
	}
}
