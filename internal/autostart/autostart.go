// Package autostart 管理开机自启。
//
// 用注册表的 Run 键而非启动文件夹:改注册表是原子的,不需要往用户
// 磁盘上放 .lnk 文件,卸载时也不留垃圾。
package autostart

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const (
	runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`
	valueName  = "PasteWin"
)

// Enable 把当前可执行文件注册为开机自启。
func Enable() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("无法定位自身路径: %w", err)
	}

	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开注册表 Run 键失败: %w", err)
	}
	defer k.Close()

	// 路径可能含空格,必须加引号,否则 Windows 会把它拆成多个参数
	if err := k.SetStringValue(valueName, `"`+exe+`"`); err != nil {
		return fmt.Errorf("写入自启项失败: %w", err)
	}
	return nil
}

// Disable 取消开机自启。
func Disable() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		// 键不存在等于本来就没开启
		return nil
	}
	defer k.Close()

	if err := k.DeleteValue(valueName); err != nil && err != registry.ErrNotExist {
		return fmt.Errorf("删除自启项失败: %w", err)
	}
	return nil
}

// IsEnabled 判断当前是否已开启自启。
//
// 只要存在同名值就算开启,不比对具体路径 —— 用户移动过 exe 时,
// 这里返回 true 让界面显示"已开启",实际路径会在下次 Enable 时刷新。
func IsEnabled() (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false, nil
	}
	defer k.Close()

	v, _, err := k.GetStringValue(valueName)
	if err == registry.ErrNotExist {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("读取自启项失败: %w", err)
	}
	return strings.TrimSpace(v) != "", nil
}

// Reconcile 让注册表与配置一致,并返回最终的实际状态。
//
// 关键取舍:当注册表里**已存在**自启项、而配置里是关闭的时候,以注册表为准
// 而不是把它删掉。原因有两个:
//
//   - 安装程序通常在应用首次启动前就写好自启项,若这里按默认配置(关闭)
//     去"纠正",安装时开启的自启会被应用第一次启动悄悄抹掉
//   - Windows 自带的「设置 → 启动应用」也允许用户改这里,
//     那个改动代表用户的最新意图,不该被应用覆盖
//
// 反方向(配置开启但注册表没有)才由应用写入,用于自愈被外部删掉的情况。
func Reconcile(want bool) (bool, error) {
	enabled, err := IsEnabled()
	if err != nil {
		return false, err
	}
	if enabled {
		return true, nil
	}
	if !want {
		return false, nil
	}
	if err := Enable(); err != nil {
		return false, err
	}
	return true, nil
}

// Sync 强制让注册表状态与期望值一致。
//
// 仅在用户在应用内切换开关时使用 —— 那是用户明确的意图。
// 启动时请用 Reconcile,它会尊重外部来源的设置。
func Sync(want bool) error {
	enabled, err := IsEnabled()
	if err != nil {
		return err
	}
	switch {
	case want && !enabled:
		return Enable()
	case !want && enabled:
		return Disable()
	}
	return nil
}
