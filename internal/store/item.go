package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"pastewin/internal/model"
)

const itemCols = `id, kind, content, plain_text, preview, blob_path, thumb_path,
	source_app, source_title, hash, pinned, pinboard_id, created_at, byte_size`

// rowScanner 抽象 *sql.Row 与 *sql.Rows 的 Scan。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanItem(sc rowScanner) (model.Item, error) {
	var (
		it        model.Item
		kind      string
		pinned    int
		created   int64
		pinboard  sql.NullString
	)
	err := sc.Scan(
		&it.ID, &kind, &it.Content, &it.PlainText, &it.Preview,
		&it.BlobPath, &it.ThumbPath, &it.SourceApp, &it.SourceTitle,
		&it.Hash, &pinned, &pinboard, &created, &it.ByteSize,
	)
	if err != nil {
		return model.Item{}, err
	}
	it.Kind = model.Kind(kind)
	it.Pinned = pinned != 0
	it.PinboardID = pinboard.String
	// created_at 以毫秒存:同一秒内连续复制多次很常见,秒级精度会让顺序变得不确定
	it.CreatedAt = time.UnixMilli(created)
	return it, nil
}

// Insert 保存一条捕获到的内容。
//
// 相同 hash 的条目不会重复插入,而是把已有条目的时间戳提到最前 ——
// 连续复制同一段文字不应在历史里堆出一串重复卡片。
// 返回的 bool 表示是否为新建条目。
func (s *Store) Insert(c model.ClipContent, blobPath, thumbPath string) (model.Item, bool, error) {
	now := time.Now().UnixMilli()

	var existingID string
	err := s.db.QueryRow(
		`SELECT id FROM items WHERE hash = ? ORDER BY created_at DESC LIMIT 1`, c.Hash,
	).Scan(&existingID)

	switch {
	case err == nil:
		// 提升时同时刷新 seq,保证它严格排到最前 —— 只靠时间戳做不到,
		// 同一毫秒内重新复制的条目会与邻居并列。
		if _, err := s.db.Exec(
			`UPDATE items SET created_at = ?, seq = (SELECT IFNULL(MAX(seq), 0) + 1 FROM items) WHERE id = ?`,
			now, existingID,
		); err != nil {
			return model.Item{}, false, fmt.Errorf("提升重复条目失败: %w", err)
		}
		it, err := s.Get(existingID)
		return it, false, err

	case errors.Is(err, sql.ErrNoRows):
		// 首次出现,继续走插入

	default:
		return model.Item{}, false, fmt.Errorf("查询重复条目失败: %w", err)
	}

	it := model.Item{
		ID:          uuid.NewString(),
		Kind:        c.Kind,
		Content:     c.Content,
		PlainText:   c.PlainText,
		Preview:     c.Preview,
		BlobPath:    blobPath,
		ThumbPath:   thumbPath,
		SourceApp:   c.SourceApp,
		SourceTitle: c.SourceTitle,
		Hash:        c.Hash,
		CreatedAt:   time.UnixMilli(now),
		ByteSize:    c.ByteSize,
	}

	_, err = s.db.Exec(
		`INSERT INTO items (`+itemCols+`, seq)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, NULL, ?, ?,
		         (SELECT IFNULL(MAX(seq), 0) + 1 FROM items))`,
		it.ID, string(it.Kind), it.Content, it.PlainText, it.Preview,
		it.BlobPath, it.ThumbPath, it.SourceApp, it.SourceTitle,
		it.Hash, now, it.ByteSize,
	)
	if err != nil {
		return model.Item{}, false, fmt.Errorf("插入条目失败: %w", err)
	}
	return it, true, nil
}

// Get 按 ID 取条目。
func (s *Store) Get(id string) (model.Item, error) {
	row := s.db.QueryRow(`SELECT `+itemCols+` FROM items WHERE id = ?`, id)
	it, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Item{}, fmt.Errorf("条目不存在: %s", id)
	}
	return it, err
}

// List 按过滤条件查询条目。
//
// Pin 的条目恒排在最前,其次按时间倒序 —— 与 Paste 的列表顺序一致。
func (s *Store) List(f model.Filter) ([]model.Item, error) {
	var (
		where []string
		args  []any
	)

	if kw := strings.TrimSpace(f.Keyword); kw != "" {
		// 在纯文本、摘要、来源应用三处模糊匹配。
		// 转义 LIKE 通配符,否则用户搜 "100%" 会变成匹配任意内容。
		like := "%" + escapeLike(kw) + "%"
		where = append(where, `(plain_text LIKE ? ESCAPE '\' OR preview LIKE ? ESCAPE '\' OR source_app LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like)
	}
	if f.Kind != "" {
		where = append(where, `kind = ?`)
		args = append(args, string(f.Kind))
	}
	if f.PinboardID != "" {
		where = append(where, `pinboard_id = ?`)
		args = append(args, f.PinboardID)
	}
	if f.SourceApp != "" {
		where = append(where, `source_app = ?`)
		args = append(args, f.SourceApp)
	}
	if f.Since > 0 {
		where = append(where, `created_at >= ?`)
		args = append(args, f.Since)
	}
	if f.Until > 0 {
		where = append(where, `created_at <= ?`)
		args = append(args, f.Until)
	}

	q := `SELECT ` + itemCols + ` FROM items`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	q += ` ORDER BY pinned DESC, seq DESC`

	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q += ` LIMIT ?`
	args = append(args, limit)
	if f.Offset > 0 {
		q += ` OFFSET ?`
		args = append(args, f.Offset)
	}

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("查询条目失败: %w", err)
	}
	defer rows.Close()

	items := make([]model.Item, 0, limit)
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, fmt.Errorf("读取条目失败: %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// Delete 删除条目并返回其 blob 相对路径,供调用方清理磁盘文件。
func (s *Store) Delete(id string) (blobPath, thumbPath string, err error) {
	it, err := s.Get(id)
	if err != nil {
		return "", "", err
	}
	if _, err := s.db.Exec(`DELETE FROM items WHERE id = ?`, id); err != nil {
		return "", "", fmt.Errorf("删除条目失败: %w", err)
	}
	return it.BlobPath, it.ThumbPath, nil
}

// TogglePin 切换置顶状态,返回切换后的值。
func (s *Store) TogglePin(id string) (bool, error) {
	it, err := s.Get(id)
	if err != nil {
		return false, err
	}
	next := !it.Pinned
	if _, err := s.db.Exec(`UPDATE items SET pinned = ? WHERE id = ?`, boolToInt(next), id); err != nil {
		return false, fmt.Errorf("更新置顶状态失败: %w", err)
	}
	return next, nil
}

// UpdateContent 就地编辑条目的文本内容。仅对文本类条目有意义。
func (s *Store) UpdateContent(id, content string) error {
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("内容不能为空")
	}
	_, err := s.db.Exec(
		`UPDATE items SET content = ?, plain_text = ?, preview = ?, byte_size = ? WHERE id = ?`,
		content, content, previewOf(content), int64(len(content)), id,
	)
	if err != nil {
		return fmt.Errorf("更新内容失败: %w", err)
	}
	return nil
}

// MoveToPinboard 把条目移入分组;pinboardID 为空表示移出分组。
func (s *Store) MoveToPinboard(itemID, pinboardID string) error {
	var arg any
	if pinboardID == "" {
		arg = nil
	} else {
		arg = pinboardID
	}
	_, err := s.db.Exec(`UPDATE items SET pinboard_id = ? WHERE id = ?`, arg, itemID)
	if err != nil {
		return fmt.Errorf("移动分组失败: %w", err)
	}
	return nil
}

// PruneResult 描述一次清理的结果。
type PruneResult struct {
	DeletedBlobs []string // 需要调用方删除的 blob 相对路径
	Count        int
}

// Prune 把未置顶、未归入分组的条目裁到 maxItems 以内,删除最旧的部分。
//
// Pin 的和进分组的属于用户明确要长期保留的内容,不参与清理。
func (s *Store) Prune(maxItems int) (PruneResult, error) {
	if maxItems <= 0 {
		return PruneResult{}, nil
	}

	// 先取出要删的条目,以便拿到 blob 路径
	rows, err := s.db.Query(`
		SELECT id, blob_path, thumb_path FROM items
		WHERE pinned = 0 AND pinboard_id IS NULL
		ORDER BY seq DESC
		LIMIT -1 OFFSET ?`, maxItems)
	if err != nil {
		return PruneResult{}, fmt.Errorf("查询待清理条目失败: %w", err)
	}

	type victim struct{ id, blob, thumb string }
	var victims []victim
	for rows.Next() {
		var v victim
		if err := rows.Scan(&v.id, &v.blob, &v.thumb); err != nil {
			rows.Close()
			return PruneResult{}, err
		}
		victims = append(victims, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return PruneResult{}, err
	}
	if len(victims) == 0 {
		return PruneResult{}, nil
	}

	res := PruneResult{Count: len(victims)}
	tx, err := s.db.Begin()
	if err != nil {
		return PruneResult{}, err
	}
	defer tx.Rollback()

	for _, v := range victims {
		if _, err := tx.Exec(`DELETE FROM items WHERE id = ?`, v.id); err != nil {
			return PruneResult{}, fmt.Errorf("清理条目失败: %w", err)
		}
		if v.blob != "" {
			res.DeletedBlobs = append(res.DeletedBlobs, v.blob)
		}
		if v.thumb != "" {
			res.DeletedBlobs = append(res.DeletedBlobs, v.thumb)
		}
	}
	return res, tx.Commit()
}

// Count 返回当前条目总数。
func (s *Store) Count() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM items`).Scan(&n)
	return n, err
}

// DistinctSourceApps 返回历史中出现过的来源应用,供搜索过滤下拉使用。
func (s *Store) DistinctSourceApps() ([]string, error) {
	rows, err := s.db.Query(`
		SELECT source_app FROM items
		WHERE source_app <> ''
		GROUP BY source_app
		ORDER BY COUNT(*) DESC
		LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var apps []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		apps = append(apps, a)
	}
	return apps, rows.Err()
}

// escapeLike 转义 LIKE 模式里的通配符,配合 ESCAPE '\' 使用。
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// previewOf 生成摘要,与 clipwatch.preview 保持一致的规则。
func previewOf(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= 200 {
		return s
	}
	return string(r[:200]) + "…"
}
