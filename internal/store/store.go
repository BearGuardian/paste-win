// Package store 负责剪贴板历史的持久化。
//
// 用 modernc.org/sqlite —— 纯 Go 实现,因为本机 CGO_ENABLED=0,
// mattn/go-sqlite3 这类 cgo 驱动无法编译。
package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// schemaVersion 记录当前表结构版本,用于后续迁移。
const schemaVersion = 1

// Store 是数据库句柄。并发安全由 database/sql 连接池保证。
type Store struct {
	db  *sql.DB
	dir string
}

// DefaultDir 返回数据目录 %LOCALAPPDATA%\PasteWin。
func DefaultDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("无法定位 LOCALAPPDATA: %w", err)
	}
	return filepath.Join(base, "PasteWin"), nil
}

// Open 打开(必要时创建)数据库。
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}

	dbPath := filepath.Join(dir, "pastewin.db")
	// DSN 里的路径必须用正斜杠,否则 Windows 反斜杠会被当作转义符
	dsn := "file:" + filepath.ToSlash(dbPath) + "?" + url.Values{
		"_pragma": {
			"journal_mode(WAL)",   // WAL 下读写不互斥,面板查询不会阻塞捕获写入
			"foreign_keys(1)",     // 删除 pinboard 时把条目的 pinboard_id 置空
			"busy_timeout(5000)",  // 遇到锁时等待而非立刻报错
			"synchronous(NORMAL)", // WAL 下的常规取舍:断电可能丢最后几条,不损坏库
		},
	}.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// SQLite 写入是串行的,限制连接数避免 "database is locked"
	db.SetMaxOpenConns(1)

	s := &Store{db: db, dir: dir}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// Dir 返回数据目录,blobs 等同级模块需要用它拼路径。
func (s *Store) Dir() string { return s.dir }

func (s *Store) migrate() error {
	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		return fmt.Errorf("读取 schema 版本失败: %w", err)
	}
	if ver >= schemaVersion {
		return nil
	}

	ddl := []string{
		`CREATE TABLE IF NOT EXISTS pinboards (
			id         TEXT PRIMARY KEY,
			name       TEXT NOT NULL,
			sort_order INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		)`,

		`CREATE TABLE IF NOT EXISTS items (
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
		)`,

		// 列表默认按时间倒序,这个索引同时服务排序与分页
		`CREATE INDEX IF NOT EXISTS idx_items_created  ON items(created_at DESC)`,
		// 历史顺序用它排序,与墙钟解耦
		`CREATE INDEX IF NOT EXISTS idx_items_seq      ON items(seq DESC)`,
		// 去重时按 hash 查已有条目
		`CREATE INDEX IF NOT EXISTS idx_items_hash     ON items(hash)`,
		// Pin 的条目固定排在最前
		`CREATE INDEX IF NOT EXISTS idx_items_pinned   ON items(pinned DESC, seq DESC)`,
		// 按分组过滤
		`CREATE INDEX IF NOT EXISTS idx_items_pinboard ON items(pinboard_id, seq DESC)`,

		`CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("开启迁移事务失败: %w", err)
	}
	defer tx.Rollback()

	for _, stmt := range ddl {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("建表失败: %w\n语句: %s", err, stmt)
		}
	}

	// PRAGMA 不支持参数占位符,只能拼接;ver 是内部常量,无注入风险
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return fmt.Errorf("写入 schema 版本失败: %w", err)
	}
	return tx.Commit()
}
