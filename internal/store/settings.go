package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"pastewin/internal/model"
)

// settingsKey 是 settings 表里存整份配置的那一行。
//
// 配置项数量少且总是整体读写,用单个 JSON 行比一列一项简单得多,
// 新增字段也不需要改表结构。
const settingsKey = "app"

// Meta 读取一个内部标记。不存在时返回 ("", false, nil)。
//
// 与用户配置分开存:这些是程序自身的状态(如 OCR 流水线版本),
// 不该出现在设置界面里,也不该被"恢复默认配置"波及。
func (s *Store) Meta(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("读取标记 %s 失败: %w", key, err)
	}
	return v, true, nil
}

// SetMeta 写入一个内部标记。
func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("写入标记 %s 失败: %w", key, err)
	}
	return nil
}

// ResetOCR 把所有图片重新标记为"未识别",返回受影响的行数。
//
// 用于 OCR 流水线升级后重做:老图片停在旧流水线的识别质量上,
// 而识别质量是会随预处理改进而变化的(见 DESIGN.md 11.6)。
func (s *Store) ResetOCR() (int64, error) {
	res, err := s.db.Exec(`UPDATE items SET ocr_done = 0 WHERE kind = 'image' AND blob_path <> ''`)
	if err != nil {
		return 0, fmt.Errorf("重置 OCR 状态失败: %w", err)
	}
	return res.RowsAffected()
}

// LoadSettings 读取配置。未初始化或解析失败时返回出厂默认值,
// 不把配置损坏升级成启动失败。
func (s *Store) LoadSettings() (model.Settings, error) {
	def := model.DefaultSettings()

	var raw string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, settingsKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	if err != nil {
		return def, fmt.Errorf("读取配置失败: %w", err)
	}

	// 从默认值出发反序列化,这样旧版本存下的配置缺字段时能自然补齐
	got := def
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		return def, fmt.Errorf("配置解析失败,已回退默认值: %w", err)
	}
	return normalize(got), nil
}

// SaveSettings 覆盖写入配置。
func (s *Store) SaveSettings(v model.Settings) error {
	v = normalize(v)
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("配置序列化失败: %w", err)
	}
	_, err = s.db.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		settingsKey, string(data),
	)
	if err != nil {
		return fmt.Errorf("保存配置失败: %w", err)
	}
	return nil
}

// normalize 修正越界或缺失的配置项,保证下游拿到的总是合法值。
func normalize(v model.Settings) model.Settings {
	def := model.DefaultSettings()

	if v.Hotkey == "" {
		v.Hotkey = def.Hotkey
	}
	if v.MaxItems <= 0 {
		v.MaxItems = def.MaxItems
	}
	// 上限过高会让面板查询与内存占用失控
	if v.MaxItems > 100000 {
		v.MaxItems = 100000
	}
	if v.PanelHeight < 120 {
		v.PanelHeight = def.PanelHeight
	}
	if v.PanelHeight > 900 {
		v.PanelHeight = 900
	}
	if v.ExcludedApps == nil {
		v.ExcludedApps = []string{}
	}
	return v
}
