package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"pastewin/internal/model"
)

// v1DDL 是引入 ocr_text / ocr_done / search_text 之前的表结构。
//
// 这里刻意把老结构抄一份而不是引用 schemaDDL —— 迁移测试要验证的正是
// "老库能升上来",如果跟着新结构一起改,测试就永远追不上真实的老库了。
const v1DDL = `
CREATE TABLE pinboards (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	sort_order INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE TABLE items (
	id           TEXT PRIMARY KEY,
	kind         TEXT NOT NULL,
	content      TEXT NOT NULL DEFAULT '',
	plain_text   TEXT NOT NULL DEFAULT '',
	preview      TEXT NOT NULL DEFAULT '',
	blob_path    TEXT NOT NULL DEFAULT '',
	thumb_path   TEXT NOT NULL DEFAULT '',
	source_app   TEXT NOT NULL DEFAULT '',
	source_title TEXT NOT NULL DEFAULT '',
	hash         TEXT NOT NULL,
	pinned       INTEGER NOT NULL DEFAULT 0,
	pinboard_id  TEXT REFERENCES pinboards(id) ON DELETE SET NULL,
	created_at   INTEGER NOT NULL,
	seq          INTEGER NOT NULL DEFAULT 0,
	byte_size    INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`

// makeV1Store 造一个 v1 版本的库,并塞进若干条目。
func makeV1Store(t *testing.T, dir string) {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "pastewin.db")))
	if err != nil {
		t.Fatalf("打开 v1 库失败: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(v1DDL); err != nil {
		t.Fatalf("建 v1 表失败: %v", err)
	}

	rows := []struct{ id, kind, plain, preview, app, blob string }{
		{"t1", "text", "消息循环检测", "消息循环检测", "chrome", ""},
		{"t2", "text", "完全无关的条目", "完全无关的条目", "notepad", ""},
		{"i1", "image", "", "图片 800 × 600", "Snipaste", "shot.png"},
	}
	for i, r := range rows {
		if _, err := db.Exec(`
			INSERT INTO items (id, kind, content, plain_text, preview, blob_path,
				thumb_path, source_app, source_title, hash, pinned, pinboard_id,
				created_at, seq, byte_size)
			VALUES (?, ?, '', ?, ?, ?, '', ?, '', ?, 0, NULL, 0, ?, 0)`,
			r.id, r.kind, r.plain, r.preview, r.blob, r.app, "hash-"+r.id, i+1,
		); err != nil {
			t.Fatalf("插入 v1 数据失败: %v", err)
		}
	}

	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatalf("写入 v1 版本号失败: %v", err)
	}
}

func TestMigrateFromV1BackfillsSearchText(t *testing.T) {
	dir := t.TempDir()
	makeV1Store(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("升级 v1 库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// 老数据必须原样还在
	n, err := s.Count()
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("升级后条目数应为 3,实际 %d", n)
	}

	// 回填过的检索文本要能支撑拼音搜索
	if got := keywordList(t, s, "消息循环检测"); len(got) != 1 {
		t.Errorf("升级后中文子串搜索失效,命中 %d 条", len(got))
	}
	if got := keywordList(t, s, "xiaoxi"); len(got) != 1 {
		t.Errorf("升级后拼音搜索失效,命中 %d 条", len(got))
	}
	if got := keywordList(t, s, "xxhjc"); len(got) != 1 {
		t.Errorf("升级后首字母搜索失效,命中 %d 条", len(got))
	}
}

func TestMigrateFromV1QueuesImagesForOCR(t *testing.T) {
	dir := t.TempDir()
	makeV1Store(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("升级 v1 库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// 历史图片应当被排进 OCR 队列(ocr_done 默认 0)
	pending, err := s.ImagesNeedingOCR(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != "i1" {
		t.Fatalf("历史图片应进入待识别队列,实际 %+v", pending)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	makeV1Store(t, dir)

	for i := 0; i < 3; i++ {
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("第 %d 次打开失败: %v", i+1, err)
		}
		n, err := s.Count()
		if err != nil {
			t.Fatal(err)
		}
		if n != 3 {
			t.Fatalf("第 %d 次打开后条目数变成 %d", i+1, n)
		}
		s.Close()
	}
}

// 全新库也要能直接建到最新版本
func TestFreshDatabaseHasCurrentSchema(t *testing.T) {
	s := newTestStore(t)

	// 新库的图片同样要能被 OCR 队列取到
	it, _, err := s.Insert(model.ClipContent{
		Kind: model.KindImage, Hash: "img-new",
	}, "n.png", "n_t.png")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.ImagesNeedingOCR(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != it.ID {
		t.Fatalf("新库的图片应进入待识别队列,实际 %+v", pending)
	}
}
