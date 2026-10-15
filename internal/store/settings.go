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
