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

// ListPinboards 返回全部分组,按自定义顺序排列,并带上各自的条目数。
func (s *Store) ListPinboards() ([]model.Pinboard, error) {
	rows, err := s.db.Query(`
		SELECT p.id, p.name, p.sort_order, p.created_at,
		       (SELECT COUNT(*) FROM items i WHERE i.pinboard_id = p.id) AS item_count
		FROM pinboards p
		ORDER BY p.sort_order ASC, p.created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("查询分组失败: %w", err)
	}
	defer rows.Close()

	boards := make([]model.Pinboard, 0, 8)
	for rows.Next() {
		var (
			b       model.Pinboard
			created int64
		)
		if err := rows.Scan(&b.ID, &b.Name, &b.SortOrder, &created, &b.ItemCount); err != nil {
			return nil, err
		}
		b.CreatedAt = time.UnixMilli(created)
		boards = append(boards, b)
	}
	return boards, rows.Err()
}

// CreatePinboard 新建分组。名称重复时返回错误。
func (s *Store) CreatePinboard(name string) (model.Pinboard, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return model.Pinboard{}, fmt.Errorf("分组名称不能为空")
	}

	var exists int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM pinboards WHERE name = ?`, name).Scan(&exists)
	if err != nil {
		return model.Pinboard{}, err
	}
	if exists > 0 {
		return model.Pinboard{}, fmt.Errorf("已存在同名分组: %s", name)
	}

	var maxOrder sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(sort_order) FROM pinboards`).Scan(&maxOrder); err != nil {
		return model.Pinboard{}, err
	}

	b := model.Pinboard{
		ID:        uuid.NewString(),
		Name:      name,
		SortOrder: int(maxOrder.Int64) + 1,
		CreatedAt: time.Now(),
	}
	_, err = s.db.Exec(
		`INSERT INTO pinboards (id, name, sort_order, created_at) VALUES (?, ?, ?, ?)`,
		b.ID, b.Name, b.SortOrder, b.CreatedAt.UnixMilli(),
	)
	if err != nil {
		return model.Pinboard{}, fmt.Errorf("创建分组失败: %w", err)
	}
	return b, nil
}

// RenamePinboard 重命名分组。
func (s *Store) RenamePinboard(id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("分组名称不能为空")
	}

	var other int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM pinboards WHERE name = ? AND id <> ?`, name, id).Scan(&other)
	if err != nil {
		return err
	}
	if other > 0 {
		return fmt.Errorf("已存在同名分组: %s", name)
	}

	res, err := s.db.Exec(`UPDATE pinboards SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return fmt.Errorf("重命名分组失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("分组不存在: %s", id)
	}
	return nil
}

// DeletePinboard 删除分组。组内条目不删除,只是解除归属(靠外键 ON DELETE SET NULL)。
func (s *Store) DeletePinboard(id string) error {
	res, err := s.db.Exec(`DELETE FROM pinboards WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删除分组失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("分组不存在: %s", id)
	}
	return nil
}

// PinboardExists 判断分组是否存在。
func (s *Store) PinboardExists(id string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM pinboards WHERE id = ?`, id).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return n > 0, err
}
